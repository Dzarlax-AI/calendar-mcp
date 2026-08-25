package google

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"calendar-mcp/internal/calendar"
)

func syncWindow() calendar.EventSyncWindow {
	return calendar.EventSyncWindow{
		Start: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),
	}
}

func TestSyncEventsReplacementUsesCompatibleParametersAndLocalProjection(t *testing.T) {
	window := syncWindow()
	var requests atomic.Int32
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		q := r.URL.Query()
		if q.Get("maxResults") != "2500" {
			t.Errorf("maxResults = %q, want 2500", q.Get("maxResults"))
		}
		if q.Get("timeMin") != "" || q.Get("timeMax") != "" {
			t.Errorf("replacement included incompatible window bounds: %q", r.URL.RawQuery)
		}
		if q.Get("singleEvents") != "true" || q.Get("showDeleted") != "true" {
			t.Errorf("replacement flags = %q", r.URL.RawQuery)
		}
		if q.Get("syncToken") != "" {
			t.Error("replacement sent syncToken")
		}
		w.Header().Set("Content-Type", "application/json")
		if q.Get("pageToken") == "page-two" {
			_, _ = io.WriteString(w, `{"items":[{"id":"second","etag":"e2","start":{"dateTime":"2026-08-20T11:00:00Z"},"end":{"dateTime":"2026-08-20T12:00:00Z"}}],"nextSyncToken":"durable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"first","etag":"e1","start":{"dateTime":"2026-08-20T09:00:00Z"},"end":{"dateTime":"2026-08-20T10:00:00Z"}},{"id":"outside","start":{"dateTime":"2026-08-22T09:00:00Z"},"end":{"dateTime":"2026-08-22T10:00:00Z"}}],"nextPageToken":"page-two"}`)
	})
	defer closeServer()

	first, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "primary", Window: window, Mode: calendar.EventSyncReplacement})
	if err != nil {
		t.Fatal(err)
	}
	if first.Complete || first.NextPageToken != "page-two" || first.NextCursor != "" || len(first.Upserts) != 1 || first.Upserts[0].Event.ID != "first" || len(first.DeletedEventIDs) != 0 {
		t.Fatalf("first page = %#v", first)
	}
	second, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "primary", Window: window, PageToken: first.NextPageToken, Mode: calendar.EventSyncReplacement})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Complete || second.NextPageToken != "" || second.NextCursor != "durable" || len(second.Upserts) != 1 || second.Upserts[0].Object.ObjectID != "second" {
		t.Fatalf("second page = %#v", second)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
}

func TestRepairEventSyncObjectQuarantinesMalformedAndConfirmsDelete(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected provider mutation: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"id":"bad","etag":"new-etag","summary":"bad"}`)
	})
	defer closeServer()
	degraded, err := provider.RepairEventSyncObject(context.Background(), calendar.EventSyncObjectRepairRequest{CalendarID: "primary", Object: calendar.SyncObject{ObjectID: "bad", ETag: "old-etag"}, Window: syncWindow()})
	if err != nil || degraded.Outcome != calendar.EventSyncObjectStillQuarantined || degraded.Warning == nil || degraded.Warning.ETag != "new-etag" {
		t.Fatalf("malformed repair=%#v err=%v", degraded, err)
	}
	deleted, err := provider.RepairEventSyncObject(context.Background(), calendar.EventSyncObjectRepairRequest{CalendarID: "primary", Object: calendar.SyncObject{ObjectID: "missing"}, Window: syncWindow()})
	if err != nil || deleted.Outcome != calendar.EventSyncObjectProviderDeleted {
		t.Fatalf("missing repair=%#v err=%v", deleted, err)
	}
}

func TestRepairEventSyncObjectCorrectsOnlyEqualAllDayDatesAndRefetches(t *testing.T) {
	var requests atomic.Int32
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch request {
		case 1:
			if r.Method != http.MethodGet {
				t.Errorf("first request method = %s, want GET", r.Method)
			}
			_, _ = io.WriteString(w, `{"id":"all-day","etag":"before","start":{"date":"2026-08-20"},"end":{"date":"2026-08-20"}}`)
		case 2:
			if r.Method != http.MethodPatch {
				t.Errorf("repair method = %s, want PATCH", r.Method)
			}
			if r.Header.Get("If-Match") != "before" {
				t.Errorf("If-Match = %q, want before", r.Header.Get("If-Match"))
			}
			if r.URL.Query().Get("sendUpdates") != "none" {
				t.Errorf("sendUpdates = %q, want none", r.URL.Query().Get("sendUpdates"))
			}
			var patch struct {
				End struct {
					Date string `json:"date"`
				} `json:"end"`
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("decode patch: %v", err)
			}
			if patch.End.Date != "2026-08-21" {
				t.Errorf("patched end date = %q, want 2026-08-21", patch.End.Date)
			}
			_, _ = io.WriteString(w, `{"id":"all-day","etag":"write-response"}`)
		case 3:
			if r.Method != http.MethodGet {
				t.Errorf("final request method = %s, want GET", r.Method)
			}
			_, _ = io.WriteString(w, `{"id":"all-day","etag":"after","start":{"date":"2026-08-20"},"end":{"date":"2026-08-21"}}`)
		default:
			t.Errorf("unexpected request %d: %s %s", request, r.Method, r.URL)
		}
	})
	defer closeServer()

	result, err := provider.RepairEventSyncObject(context.Background(), calendar.EventSyncObjectRepairRequest{CalendarID: "primary", Object: calendar.SyncObject{ObjectID: "all-day"}, Window: syncWindow()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != calendar.EventSyncObjectReplaceMembership || result.Object.ETag != "after" || len(result.Upserts) != 1 || result.Upserts[0].Event.Start.Date != "2026-08-20" || result.Upserts[0].Event.End.Date != "2026-08-21" {
		t.Fatalf("repair result = %#v", result)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests = %d, want get, patch, get", requests.Load())
	}
}

func TestRepairEventSyncObjectLeavesOtherInvalidRangesUnchanged(t *testing.T) {
	for _, tt := range []struct {
		name   string
		body   string
		reason string
	}{
		{name: "timed", body: `{"id":"bad","etag":"etag","start":{"dateTime":"2026-08-20T09:00:00Z"},"end":{"dateTime":"2026-08-20T09:00:00Z"}}`, reason: "invalid_time_range"},
		{name: "reversed all day", body: `{"id":"bad","etag":"etag","start":{"date":"2026-08-20"},"end":{"date":"2026-08-19"}}`, reason: "invalid_time_range"},
		{name: "recurring", body: `{"id":"bad","etag":"etag","recurrence":["RRULE:FREQ=DAILY"],"start":{"date":"2026-08-20"},"end":{"date":"2026-08-20"}}`, reason: "invalid_time_range"},
		{name: "special type", body: `{"id":"bad","etag":"etag","eventType":"birthday","start":{"date":"2026-08-20"},"end":{"date":"2026-08-20"}}`, reason: "invalid_time_range"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var patches atomic.Int32
			provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patches.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			})
			defer closeServer()

			result, err := provider.RepairEventSyncObject(context.Background(), calendar.EventSyncObjectRepairRequest{CalendarID: "primary", Object: calendar.SyncObject{ObjectID: "bad"}, Window: syncWindow()})
			if err != nil || result.Outcome != calendar.EventSyncObjectStillQuarantined || result.Warning == nil || result.Warning.Diagnostic == nil || result.Warning.Diagnostic.ProviderReason != tt.reason {
				t.Fatalf("repair result=%#v err=%v", result, err)
			}
			if patches.Load() != 0 {
				t.Fatalf("patches = %d, want 0", patches.Load())
			}
		})
	}
}

func TestRepairEventSyncObjectReturnsConditionalUpdateConflict(t *testing.T) {
	var requests atomic.Int32
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if request == 1 {
			_, _ = io.WriteString(w, `{"id":"all-day","etag":"before","start":{"date":"2026-08-20"},"end":{"date":"2026-08-20"}}`)
			return
		}
		if r.Method != http.MethodPatch || r.Header.Get("If-Match") != "before" {
			t.Errorf("conflict request = %s If-Match=%q", r.Method, r.Header.Get("If-Match"))
		}
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"error":{"code":412,"errors":[{"reason":"conditionNotMet"}]}}`)
	})
	defer closeServer()

	_, err := provider.RepairEventSyncObject(context.Background(), calendar.EventSyncObjectRepairRequest{CalendarID: "primary", Object: calendar.SyncObject{ObjectID: "all-day"}, Window: syncWindow()})
	var syncErr *calendar.EventSyncError
	if !errors.As(err, &syncErr) || syncErr.Class != calendar.EventSyncProtocol || syncErr.ProviderStatus != http.StatusPreconditionFailed || syncErr.ProviderReason != "conditionNotMet" {
		t.Fatalf("error = %#v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want get and failed patch", requests.Load())
	}
}

func TestSyncEventsIncrementalOmitsWindowAndUsesTerminalToken(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("syncToken") != "opaque-cursor" || q.Get("pageToken") != "page-two" {
			t.Errorf("incremental token params = %q", r.URL.RawQuery)
		}
		if q.Get("timeMin") != "" || q.Get("timeMax") != "" {
			t.Errorf("incremental included replacement window: %q", r.URL.RawQuery)
		}
		if q.Get("singleEvents") != "true" || q.Get("showDeleted") != "true" {
			t.Errorf("incremental flags = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"nextSyncToken":"terminal-cursor"}`)
	})
	defer closeServer()

	page, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "primary", Window: syncWindow(), Cursor: "opaque-cursor", PageToken: "page-two", Mode: calendar.EventSyncIncremental})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete || page.NextCursor != "terminal-cursor" || page.NextPageToken != "" {
		t.Fatalf("page = %#v", page)
	}
}

func TestSyncEventsRejectsInvalidIncrementalWindowBeforeAdvancingCursor(t *testing.T) {
	var requests atomic.Int32
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"nextSyncToken":"must-not-be-used"}`)
	})
	defer closeServer()

	page, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{
		CalendarID: "primary",
		Window:     calendar.EventSyncWindow{Start: syncWindow().End, End: syncWindow().Start},
		Cursor:     "opaque-cursor",
		Mode:       calendar.EventSyncIncremental,
	})
	var syncErr *calendar.EventSyncError
	if !errors.As(err, &syncErr) || syncErr.Class != calendar.EventSyncProtocol {
		t.Fatalf("error = %#v, want protocol error", err)
	}
	if page.Complete || page.NextCursor != "" || requests.Load() != 0 {
		t.Fatalf("page = %#v, requests = %d; invalid window must not advance or call Google", page, requests.Load())
	}
}

func TestSyncEventsCancellationAndMovedEventBecomeDeletions(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"cancelled","status":"cancelled"},{"id":"moved","start":{"dateTime":"2026-08-22T09:00:00Z"},"end":{"dateTime":"2026-08-22T10:00:00Z"}}],"nextSyncToken":"next"}`)
	})
	defer closeServer()

	page, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "primary", Window: syncWindow(), Cursor: "cursor", Mode: calendar.EventSyncIncremental})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Upserts) != 0 || !sameStrings(page.DeletedEventIDs, []string{"cancelled", "moved"}) {
		t.Fatalf("page = %#v", page)
	}
}

func TestSyncEventsIsolatesMalformedEventFromValidPage(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("normal sync must not mutate provider data: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"valid","start":{"dateTime":"2026-08-20T09:00:00Z"},"end":{"dateTime":"2026-08-20T10:00:00Z"}},{"id":"malformed","start":{"dateTime":"not-a-time"},"end":{"dateTime":"not-a-time"}}],"nextSyncToken":"next"}`)
	})
	defer closeServer()

	page, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "primary", Window: syncWindow(), Mode: calendar.EventSyncReplacement})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete || page.NextCursor != "next" || len(page.Upserts) != 1 || page.Upserts[0].Event.ID != "valid" {
		t.Fatalf("page = %#v", page)
	}
	if len(page.Warnings) != 1 || page.Warnings[0].Code != calendar.EventSyncProtocol || page.Warnings[0].ObjectID != "malformed" || page.Warnings[0].Diagnostic == nil || page.Warnings[0].Diagnostic.ProviderReason != "invalid_start" || len(page.Warnings[0].Diagnostic.RawPayload) == 0 {
		t.Fatalf("warnings = %#v", page.Warnings)
	}
	var diagnostic struct {
		Start struct {
			DateTime string `json:"dateTime"`
		} `json:"start"`
		End struct {
			DateTime string `json:"dateTime"`
		} `json:"end"`
	}
	if err := json.Unmarshal(page.Warnings[0].Diagnostic.RawPayload, &diagnostic); err != nil || diagnostic.Start.DateTime != "not-a-time" || diagnostic.End.DateTime != "not-a-time" {
		t.Fatalf("diagnostic payload = %s, err = %v", page.Warnings[0].Diagnostic.RawPayload, err)
	}
}

func TestSyncEventsStaleCursorRequestsResetWithoutMutations(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"error":{"code":410,"message":"gone"}}`)
	})
	defer closeServer()

	page, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "primary", Window: syncWindow(), Cursor: "secret", Mode: calendar.EventSyncIncremental})
	if err != nil {
		t.Fatal(err)
	}
	if !page.ResetRequired || page.Complete || len(page.Upserts) != 0 || len(page.DeletedEventIDs) != 0 || page.NextCursor != "" || page.NextPageToken != "" {
		t.Fatalf("reset page = %#v", page)
	}
}

func TestSyncEventsClassifiesErrorsWithoutLeakingTokens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		code   int
		reason string
		want   calendar.EventSyncErrorClass
	}{
		{name: "rate limit", code: http.StatusTooManyRequests, want: calendar.EventSyncRateLimited},
		{name: "rate limit reported as forbidden", code: http.StatusForbidden, reason: "userRateLimitExceeded", want: calendar.EventSyncRateLimited},
		{name: "forbidden", code: http.StatusForbidden, reason: "forbidden", want: calendar.EventSyncPermission},
		{name: "auth", code: http.StatusUnauthorized, want: calendar.EventSyncAuth},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider, closeServer := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tt.code)
				_, _ = io.WriteString(w, `{"error":{"code":`+strconv.Itoa(tt.code)+`,"message":"opaque-cursor opaque-page","errors":[{"reason":"`+tt.reason+`"}]}}`)
			})
			defer closeServer()

			_, err := provider.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "primary", Window: syncWindow(), Cursor: "opaque-cursor", PageToken: "opaque-page", Mode: calendar.EventSyncIncremental})
			var syncErr *calendar.EventSyncError
			if !errors.As(err, &syncErr) || syncErr.Class != tt.want {
				t.Fatalf("error = %#v, want class %q", err, tt.want)
			}
			if syncErr.ProviderStatus != tt.code || syncErr.ProviderReason != tt.reason {
				t.Fatalf("provider detail = status %d reason %q, want %d %q", syncErr.ProviderStatus, syncErr.ProviderReason, tt.code, tt.reason)
			}
			if tt.want == calendar.EventSyncRateLimited && syncErr.RetryAfter != 7*time.Second {
				t.Fatalf("RetryAfter = %s, want 7s", syncErr.RetryAfter)
			}
			if got := err.Error(); strings.Contains(got, "opaque-cursor") || strings.Contains(got, "opaque-page") {
				t.Fatalf("error leaked opaque token: %q", got)
			}
		})
	}
}

func TestClassifyGoogleSyncErrorPreservesOAuthRefreshDetails(t *testing.T) {
	err := classifyGoogleSyncError(&oauth2.RetrieveError{
		Response:         &http.Response{StatusCode: http.StatusUnauthorized},
		ErrorCode:        "invalid_grant",
		ErrorDescription: "redacted description",
	})
	var syncErr *calendar.EventSyncError
	if !errors.As(err, &syncErr) || syncErr.Class != calendar.EventSyncAuth {
		t.Fatalf("error=%#v, want auth EventSyncError", err)
	}
	if syncErr.ProviderStatus != http.StatusUnauthorized || syncErr.ProviderReason != "invalid_grant" {
		t.Fatalf("provider detail = status %d reason %q", syncErr.ProviderStatus, syncErr.ProviderReason)
	}
	if strings.Contains(err.Error(), "redacted description") {
		t.Fatalf("error leaked OAuth description: %q", err)
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
