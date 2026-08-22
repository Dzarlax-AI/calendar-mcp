package microsoft

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func syncWindow() calendar.EventSyncWindow {
	return calendar.EventSyncWindow{Start: time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)}
}

func syncEventJSON(id, start, end string) string {
	return fmt.Sprintf(`{"id":%q,"subject":"Event","start":{"dateTime":%q,"timeZone":"UTC"},"end":{"dateTime":%q,"timeZone":"UTC"}}`, id, start, end)
}

func TestMicrosoftEventSyncReplacementStartsDeltaWithFrozenWindow(t *testing.T) {
	window := syncWindow()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/calendars/team/calendarView/delta" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("startDateTime"); got != window.Start.Format(time.RFC3339) {
			t.Fatalf("startDateTime = %q", got)
		}
		if got := r.URL.Query().Get("endDateTime"); got != window.End.Format(time.RFC3339) {
			t.Fatalf("endDateTime = %q", got)
		}
		_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"`+serverURL(r)+`/delta?token=opaque"}`)
	}))
	defer server.Close()

	page, err := (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: window, Mode: calendar.EventSyncReplacement})
	if err != nil || !page.Complete || page.NextCursor == "" {
		t.Fatalf("page=%#v err=%v", page, err)
	}
}

func TestMicrosoftEventSyncDrainsNextLinkAndReturnsDeltaLink(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/calendars/team/calendarView/delta":
			_, _ = fmt.Fprintf(w, `{"value":[%s],"@odata.nextLink":%q}`, syncEventJSON("one", "2026-08-22T10:00:00", "2026-08-22T11:00:00"), server.URL+"/page-two?opaque=next")
		case "/page-two":
			_, _ = fmt.Fprintf(w, `{"value":[%s],"@odata.deltaLink":%q}`, syncEventJSON("two", "2026-08-22T12:00:00", "2026-08-22T13:00:00"), server.URL+"/delta?opaque=cursor")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &Provider{client: server.Client(), baseURL: server.URL}
	first, err := p.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncReplacement})
	if err != nil || first.Complete || first.NextPageToken == "" || len(first.Upserts) != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := p.SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncReplacement, PageToken: first.NextPageToken})
	if err != nil || !second.Complete || second.NextCursor == "" || len(second.Upserts) != 1 || second.Upserts[0].Event.ID != "two" {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}

func TestMicrosoftEventSyncDeletesRemovedAndMovedOutsideWindow(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"value":[{"id":"removed","@removed":{"reason":"deleted"}},%s],"@odata.deltaLink":%q}`, syncEventJSON("moved", "2026-08-22T18:00:00", "2026-08-22T19:00:00"), server.URL+"/delta?opaque=cursor")
	}))
	defer server.Close()
	page, err := (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncReplacement})
	if err != nil || len(page.Upserts) != 0 || len(page.DeletedEventIDs) != 2 || page.DeletedEventIDs[0] != "removed" || page.DeletedEventIDs[1] != "moved" {
		t.Fatalf("page=%#v err=%v", page, err)
	}
}

func TestMicrosoftEventSyncWindowUsesHalfOpenOverlap(t *testing.T) {
	window := syncWindow()
	for _, test := range []struct {
		name  string
		event calendar.EventV2
		want  bool
	}{
		{
			name:  "timed event spanning window start",
			event: calendar.EventV2{Start: calendar.EventTime{DateTime: "2026-08-22T08:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T10:00:00Z", TimeZone: "UTC"}},
			want:  true,
		},
		{
			name:  "all day event spanning window start",
			event: calendar.EventV2{Start: calendar.EventTime{Date: "2026-08-21"}, End: calendar.EventTime{Date: "2026-08-23"}},
			want:  true,
		},
		{
			name:  "event ending at window start does not overlap",
			event: calendar.EventV2{Start: calendar.EventTime{DateTime: "2026-08-22T08:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T09:00:00Z", TimeZone: "UTC"}},
			want:  false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := eventV2InSyncWindow(test.event, window)
			if err != nil || got != test.want {
				t.Fatalf("inWindow=%v err=%v, want %v", got, err, test.want)
			}
		})
	}
}

func TestMicrosoftEventSyncMalformedTimesAreProtocolErrors(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"value":[{"id":"invalid","start":{"dateTime":"not-a-time","timeZone":"UTC"},"end":{"dateTime":"2026-08-22T10:00:00","timeZone":"UTC"}}],"@odata.deltaLink":%q}`, server.URL+"/delta?opaque=cursor")
	}))
	defer server.Close()
	page, err := (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncReplacement})
	if len(page.Upserts) != 0 || len(page.DeletedEventIDs) != 0 || page.NextPageToken != "" || page.NextCursor != "" || page.Complete || page.ResetRequired {
		t.Fatalf("malformed event produced mutations: %#v", page)
	}
	var syncErr *calendar.EventSyncError
	if !errorsAsSync(err, &syncErr) || syncErr.Class != calendar.EventSyncProtocol {
		t.Fatalf("error = %#v", err)
	}
}

func TestMicrosoftEventSyncPolicy(t *testing.T) {
	policy := (&Provider{}).EventSyncPolicy()
	if policy.PollInterval != time.Minute || policy.RetryBase != 5*time.Second || policy.RetryMax != 5*time.Minute || policy.MaxPages != 250 || policy.MaxResets != 2 {
		t.Fatalf("policy = %#v", policy)
	}
}

func TestMicrosoftEventSyncInvalidDeltaResetsWithoutMutations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"error":{"code":"syncStateNotFound"}}`)
	}))
	defer server.Close()
	page, err := (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncIncremental, Cursor: calendar.EventSyncCursor(server.URL + "/delta?opaque=cursor")})
	if err != nil || !page.ResetRequired || len(page.Upserts) != 0 || page.NextCursor != "" || page.NextPageToken != "" {
		t.Fatalf("page=%#v err=%v", page, err)
	}
}

func TestMicrosoftEventSyncRejectsHostileLinksWithoutLeakingOpaqueValues(t *testing.T) {
	const secret = "untrusted-opaque-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/calendars/delta/") {
			_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"https://example.test/delta?token=`+secret+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"value":[],"@odata.nextLink":"https://example.test/delta?token=`+secret+`"}`)
	}))
	defer server.Close()
	_, err := (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncReplacement})
	if err == nil || !strings.Contains(err.Error(), string(calendar.EventSyncProtocol)) || strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks or misclassifies opaque link: %v", err)
	}
	_, err = (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "delta", Window: syncWindow(), Mode: calendar.EventSyncReplacement})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("delta link error leaks opaque value: %v", err)
	}

	_, err = (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncIncremental, Cursor: calendar.EventSyncCursor("https://example.test/delta?token=" + secret)})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("cursor error leaks opaque value: %v", err)
	}
}

func TestMicrosoftEventSyncClassifiesAuthAndRateLimit(t *testing.T) {
	for _, test := range []struct {
		name, path string
		status     int
		want       calendar.EventSyncErrorClass
	}{
		{name: "auth", path: "/auth", status: http.StatusUnauthorized, want: calendar.EventSyncAuth},
		{name: "rate limit", path: "/rate", status: http.StatusTooManyRequests, want: calendar.EventSyncRateLimited},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "12")
				}
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			_, err := (&Provider{client: server.Client(), baseURL: server.URL}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "team", Window: syncWindow(), Mode: calendar.EventSyncIncremental, Cursor: calendar.EventSyncCursor(server.URL + test.path)})
			var syncErr *calendar.EventSyncError
			if !errorsAsSync(err, &syncErr) || syncErr.Class != test.want {
				t.Fatalf("error = %#v", err)
			}
			if test.want == calendar.EventSyncRateLimited && syncErr.RetryAfter != 12*time.Second {
				t.Fatalf("retry after = %s", syncErr.RetryAfter)
			}
		})
	}
}

func serverURL(r *http.Request) string { return "http://" + r.Host }

func errorsAsSync(err error, target **calendar.EventSyncError) bool {
	value, ok := err.(*calendar.EventSyncError)
	if ok {
		*target = value
	}
	return ok
}
