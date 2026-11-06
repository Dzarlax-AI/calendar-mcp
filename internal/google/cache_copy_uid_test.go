package google

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/syncengine"
)

func TestGoogleCacheCopyUsesAssignedUIDAndStableMarker(t *testing.T) {
	for _, lostResponse := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-response=", lostResponse), func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			start := calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
			end := calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
			event := calendar.EventV2{ID: "source-occurrence", CalendarID: "google:source", ICalUID: "source-series-uid", Status: "confirmed", Title: "Synthetic copy", Start: start, End: end, RecurringEventID: "source-master", OriginalStart: &start}
			reader := &syntheticCacheHTTPSource{value: storage.SourceProjectionForCopy{Snapshot: storage.ProjectionSnapshot{CalendarID: event.CalendarID, Revision: 1, Timezone: "UTC", Verified: true}, Events: []calendar.EventV2{event}}}
			var active map[string]any
			var sourceMarker, retiredUID string
			posts := 0
			var mu sync.Mutex
			state := func() (int, bool) {
				mu.Lock()
				defer mu.Unlock()
				return posts, active != nil
			}
			p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
					marker := strings.TrimPrefix(r.URL.Query().Get("privateExtendedProperty"), "calendar_source_event=")
					if marker == "" || r.URL.Query().Get("showDeleted") != "false" {
						t.Error("missing active-marker lookup")
					}
					if sourceMarker == "" {
						sourceMarker, retiredUID = marker, marker+"@calendar-mcp"
					} else if sourceMarker != marker {
						t.Error("copy identity changed")
					}
					// The previous object is a tombstone, invisible to active lookup.
					items := []any{}
					if active != nil {
						items = append(items, active)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("unexpected request %s", r.Method)
					w.WriteHeader(500)
					return
				}
				posts++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["iCalUID"] == retiredUID {
					w.WriteHeader(http.StatusConflict)
					fmt.Fprint(w, `{"error":{"code":409,"errors":[{"reason":"duplicate"}]}}`)
					return
				}
				for _, field := range []string{"id", "iCalUID", "attendees", "recurrence", "conferenceData"} {
					if _, exists := body[field]; exists {
						t.Errorf("cache copy unexpectedly supplied %s", field)
					}
				}
				if r.URL.Query().Get("sendUpdates") != "none" {
					t.Error("notifications not suppressed")
				}
				extended, _ := body["extendedProperties"].(map[string]any)
				private, _ := extended["private"].(map[string]any)
				if private["calendar_sync_rule"] != "rule" || private["calendar_source_event"] != sourceMarker {
					t.Error("stable ownership marker lost")
				}
				body["id"], body["iCalUID"], body["etag"] = "new-google-target", "google-assigned-uid", `"new-etag"`
				body["status"], body["organizer"] = "confirmed", map[string]any{"self": true}
				active = body
				if lostResponse && posts == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				_ = json.NewEncoder(w).Encode(body)
			})
			defer closeServer()
			mappings := &syntheticSyncMappings{values: map[string]storage.Mapping{}}
			engine := syncengine.New(calendar.NewRegistry([]calendar.Provider{p}), mappings).WithDispatchFence(syntheticDispatchFence{})
			rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "google:target", State: "enabled", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
			_, err := engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour)
			count, hasActive := state()
			if (err != nil) != lostResponse || count != 1 || !hasActive {
				t.Fatalf("first creation posts=%d err=%v", count, err)
			}
			if lostResponse {
				for _, mapping := range mappings.values {
					if mapping.ReconciliationState != "pending_create" || !strings.HasPrefix(mapping.TargetEventID, "pending:") {
						t.Fatal("lost reply did not retain creation intent")
					}
				}
			} else {
				// Losing local mappings still recovers the active copy by marker,
				// never by the unrelated server-assigned UID or retired UID.
				mappings.values = map[string]storage.Mapping{}
			}
			_, err = engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour)
			count, _ = state()
			if err != nil || count != 1 || len(mappings.values) != 1 {
				t.Fatalf("marker recovery posts=%d err=%v", count, err)
			}
			for _, mapping := range mappings.values {
				if mapping.ReconciliationState != "current" || mapping.TargetEventID != "new-google-target" || mapping.TargetETag != `"new-etag"` {
					t.Fatalf("unconfirmed recovered mapping: %+v", mapping)
				}
			}
			_, err = engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour)
			count, _ = state()
			if err != nil || count != 1 {
				t.Fatalf("repeat duplicated copy: posts=%d err=%v", count, err)
			}
		})
	}
}

func TestGoogleCacheCopyLostBeforeApplyNeverRepeatsPOST(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	event := calendar.EventV2{ID: "source", CalendarID: "google:source", Status: "confirmed", Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	reader := &syntheticCacheHTTPSource{value: storage.SourceProjectionForCopy{Snapshot: storage.ProjectionSnapshot{CalendarID: event.CalendarID, Revision: 1, Timezone: "UTC", Verified: true}, Events: []calendar.EventV2{event}}}
	var mu sync.Mutex
	posts := 0
	p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(500)
			return
		}
		posts++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if _, exists := body["iCalUID"]; exists || r.URL.Query().Get("sendUpdates") != "none" {
			t.Error("unexpected UID or notification policy")
		}
		// Model a lost request before the fake provider applies anything.
		// The client cannot distinguish this from a lost successful reply.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	defer closeServer()
	mappings := &syntheticSyncMappings{values: map[string]storage.Mapping{}}
	engine := syncengine.New(calendar.NewRegistry([]calendar.Provider{p}), mappings).WithDispatchFence(syntheticDispatchFence{})
	rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "google:target", State: "enabled", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour); err == nil {
		t.Fatal("lost request reported success")
	}
	if _, err := engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour); !errors.Is(err, syncengine.ErrPendingCreateUnresolved) {
		t.Fatalf("negative lookup did not remain unresolved: %v", err)
	}
	mu.Lock()
	count := posts
	mu.Unlock()
	if count != 1 || len(mappings.values) != 1 {
		t.Fatalf("blind retry: posts=%d mappings=%d", count, len(mappings.values))
	}
	for _, mapping := range mappings.values {
		if mapping.ReconciliationState != "pending_create" || mapping.Version != 1 || !strings.HasPrefix(mapping.TargetEventID, "pending:") {
			t.Fatal("creation intent was cleared without provider evidence")
		}
	}
}
