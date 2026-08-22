package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

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
	requests := 0
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
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
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
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
			if tt.want == calendar.EventSyncRateLimited && syncErr.RetryAfter != 7*time.Second {
				t.Fatalf("RetryAfter = %s, want 7s", syncErr.RetryAfter)
			}
			if got := err.Error(); strings.Contains(got, "opaque-cursor") || strings.Contains(got, "opaque-page") {
				t.Fatalf("error leaked opaque token: %q", got)
			}
		})
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
