package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/syncengine"
)

type syntheticCacheHTTPSource struct {
	value storage.SourceProjectionForCopy
}

func (s *syntheticCacheHTTPSource) GetSourceProjectionForCopy(context.Context, string, time.Time, time.Time, time.Time, time.Duration) (storage.SourceProjectionForCopy, error) {
	return s.value, nil
}
func (s *syntheticCacheHTTPSource) CheckSourceProjectionCurrent(_ context.Context, _ string, revision int64, _ time.Time, _ time.Time, _ time.Time, _ time.Duration) error {
	if revision != s.value.Snapshot.Revision {
		return storage.ErrProjectionSnapshotUnavailable
	}
	return nil
}
func (s *syntheticCacheHTTPSource) CheckSyncTargetEligibility(context.Context, string) error {
	return nil
}

// The provider is the real Google adapter; only its HTTP transport and the
// source snapshot are synthetic. Google omits attendees entirely on GET.
func TestCacheCopyGoogleHTTPCreateUpdateDeleteWithoutSourceReads(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	end := calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	event := calendar.EventV2{ID: "source-event", CalendarID: "google:source", Title: "first", Start: start, End: end, SourceAttendeesComplete: true}
	identity, err := calendarprojection.Identity(event.CalendarID, event)
	if err != nil {
		t.Fatal(err)
	}
	reader := &syntheticCacheHTTPSource{value: storage.SourceProjectionForCopy{Snapshot: storage.ProjectionSnapshot{CalendarID: event.CalendarID, Revision: 1, Timezone: "UTC", Verified: true}, Events: []calendar.EventV2{event}}}
	var marker map[string]string
	version := `"v1"`
	posts, gets, puts, deletes, sourceCalls := 0, 0, 0, 0, 0
	p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/calendars/source/") {
			sourceCalls++
			t.Error("source API was called")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Query().Get("sendUpdates") != "none" && r.Method != http.MethodGet {
			t.Errorf("notifications not suppressed on %s", r.Method)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = io.WriteString(w, `{"items":[]}`)
		case r.Method == http.MethodPost:
			posts++
			var body struct {
				Attendees json.RawMessage `json:"attendees"`
				Extended  struct {
					Private map[string]string `json:"private"`
				} `json:"extendedProperties"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if len(body.Attendees) != 0 {
				t.Error("create sent attendees")
			}
			marker = body.Extended.Private
			if marker["calendar_sync_rule"] != "rule" || marker["calendar_source_event"] == "" {
				t.Error("create did not carry marker")
			}
			response, _ := json.Marshal(map[string]any{"id": "target-event", "etag": version, "eventType": "default", "organizer": map[string]any{"self": true}, "extendedProperties": map[string]any{"private": marker}, "start": map[string]any{"dateTime": start.DateTime, "timeZone": "UTC"}, "end": map[string]any{"dateTime": end.DateTime, "timeZone": "UTC"}})
			_, _ = w.Write(response)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/target-event"):
			gets++
			response, _ := json.Marshal(map[string]any{"id": "target-event", "etag": version, "eventType": "default", "organizer": map[string]any{"self": true}, "extendedProperties": map[string]any{"private": marker}, "start": map[string]any{"dateTime": start.DateTime, "timeZone": "UTC"}, "end": map[string]any{"dateTime": end.DateTime, "timeZone": "UTC"}})
			_, _ = w.Write(response)
		case r.Method == http.MethodPut:
			puts++
			if r.Header.Get("If-Match") != version {
				t.Errorf("update If-Match=%q want=%q", r.Header.Get("If-Match"), version)
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if _, ok := body["attendees"]; ok {
				t.Error("update sent attendees")
			}
			version = `"v2"`
			response, _ := json.Marshal(map[string]any{"id": "target-event", "etag": version, "extendedProperties": map[string]any{"private": marker}, "start": map[string]any{"dateTime": start.DateTime, "timeZone": "UTC"}, "end": map[string]any{"dateTime": end.DateTime, "timeZone": "UTC"}})
			_, _ = w.Write(response)
		case r.Method == http.MethodDelete:
			deletes++
			if r.Header.Get("If-Match") != version {
				t.Errorf("delete If-Match=%q want=%q", r.Header.Get("If-Match"), version)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer closeServer()
	mappings := &syntheticSyncMappings{values: map[string]storage.Mapping{}}
	engine := syncengine.New(calendar.NewRegistry([]calendar.Provider{p}), mappings).WithDispatchFence(syntheticDispatchFence{})
	rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "google:target", State: "enabled", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err = engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour); err != nil {
		t.Fatalf("create: %v", err)
	}
	event.Title = "changed"
	reader.value.Events = []calendar.EventV2{event}
	reader.value.Snapshot.Revision = 2
	// Independent synthetic attempts model a manual edit and a foreign marker.
	// They must stop before PUT and retain the uncertain mapping intent.
	cloneMappings := func() *syntheticSyncMappings {
		copy := &syntheticSyncMappings{values: map[string]storage.Mapping{}}
		for id, value := range mappings.values {
			copy.values[id] = value
		}
		return copy
	}
	for _, changed := range []string{"etag", "marker"} {
		attempt := cloneMappings()
		candidate := syncengine.New(calendar.NewRegistry([]calendar.Provider{p}), attempt).WithDispatchFence(syntheticDispatchFence{})
		oldVersion, oldRule := version, marker["calendar_sync_rule"]
		if changed == "etag" {
			version = `"manual"`
		} else {
			marker["calendar_sync_rule"] = "foreign-rule"
		}
		_, err = candidate.RunCacheCopy(t.Context(), reader, rule, now, time.Hour)
		version, marker["calendar_sync_rule"] = oldVersion, oldRule
		if err == nil || puts != 0 {
			t.Fatalf("%s change was overwritten: err=%v puts=%d", changed, err, puts)
		}
		for _, value := range attempt.values {
			if value.ReconciliationState != "pending_update" {
				t.Fatalf("%s change did not remain pending", changed)
			}
		}
	}
	if _, err = engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour); err != nil {
		t.Fatalf("update: %v", err)
	}
	reader.value.Events = nil
	reader.value.Snapshot.Revision = 3
	reader.value.Observations = []calendarprojection.RemovalObservation{{Removal: calendar.EventSyncRemoval{ObjectID: event.ID, Reason: calendar.EventSyncProviderCancellation}, PreviousIdentity: identity}}
	if _, err = engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if posts != 1 || gets != 4 || puts != 1 || deletes != 1 || sourceCalls != 0 || len(mappings.values) != 0 {
		t.Fatal(fmt.Sprintf("posts=%d gets=%d puts=%d deletes=%d source=%d mappings=%d", posts, gets, puts, deletes, sourceCalls, len(mappings.values)))
	}
}
