package apple

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

const eventSyncCalendar = "/calendar/"

func eventSyncProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	client, err := caldav.NewClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Provider{client: client, httpClient: server.Client(), caldavURL: server.URL}
}

func eventSyncRequest(mode calendar.EventSyncMode) calendar.EventSyncRequest {
	return calendar.EventSyncRequest{
		CalendarID: eventSyncCalendar,
		Window:     calendar.EventSyncWindow{Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		Mode:       mode,
	}
}

func syncResponse(token string, limited bool, responses string) string {
	limit := ""
	if limited {
		limit = "<D:number-of-matches-within-limits/>"
	}
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">` + responses + `<D:sync-token>` + token + `</D:sync-token>` + limit + `</D:multistatus>`
}

func changedObject(path, etag string) string {
	return `<D:response><D:href>` + path + `</D:href><D:propstat><D:prop><D:getetag>` + etag + `</D:getetag></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func inventoryResponse(entries string) string {
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">` + entries + `</D:multistatus>`
}

func queryResponse(path, etag, data string) string {
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:response><D:href>` + path + `</D:href><D:propstat><D:prop><D:getetag>` + etag + `</D:getetag><C:calendar-data>` + data + `</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
}

func eventObject(uid string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:" + uid + "\r\nDTSTART:20260820T090000Z\r\nDTEND:20260820T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func TestAppleEventSyncCollectionPagesAndKeepsTokensOutOfErrors(t *testing.T) {
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			body, _ := io.ReadAll(r.Body)
			if reports.Add(1) == 1 {
				if !strings.Contains(string(body), "old-token") {
					t.Error("first REPORT did not carry the saved cursor")
				}
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = w.Write([]byte(syncResponse("page-token", true, changedObject("/calendar/one.ics", `"etag-1"`))))
				return
			}
			if !strings.Contains(string(body), "page-token") {
				t.Error("continued REPORT did not carry the page token")
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(syncResponse("next-token", false, changedObject("/calendar/two.ics", `"etag-2"`))))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte(eventObject(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/calendar/"), ".ics"))))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	p := eventSyncProvider(t, server)

	request := eventSyncRequest(calendar.EventSyncIncremental)
	request.Cursor = "old-token"
	first, err := p.SyncEvents(context.Background(), request)
	if err != nil {
		t.Fatalf("first SyncEvents() error = %v", err)
	}
	if first.NextPageToken != "page-token" || first.Complete || first.NextCursor != "" || len(first.Upserts) != 1 || first.Upserts[0].Object.ETag != `"etag-1"` {
		t.Fatalf("first page = %#v", first)
	}
	request.PageToken = first.NextPageToken
	second, err := p.SyncEvents(context.Background(), request)
	if err != nil {
		t.Fatalf("second SyncEvents() error = %v", err)
	}
	if !second.Complete || second.NextCursor != "next-token" || second.NextPageToken != "" || len(second.Upserts) != 1 || second.Upserts[0].Object.ETag != `"etag-2"` {
		t.Fatalf("second page = %#v", second)
	}

	bad := eventSyncRequest(calendar.EventSyncIncremental)
	bad.Cursor = "secret-token"
	bad.CalendarID = ""
	_, err = p.SyncEvents(context.Background(), bad)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error leaked cursor or was nil: %v", err)
	}
}

func TestAppleRepairMalformedUsesResponseETag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/calendar")
			w.Header().Set("ETag", `"changed"`)
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nBROKEN\r\nEND:VCALENDAR\r\n"))
		}
	}))
	defer server.Close()
	result, err := eventSyncProvider(t, server).RepairEventSyncObject(context.Background(), calendar.EventSyncObjectRepairRequest{CalendarID: eventSyncCalendar, Object: calendar.SyncObject{ObjectID: "/calendar/bad.ics", ETag: `"old"`}, Window: eventSyncRequest(calendar.EventSyncIncremental).Window})
	if err != nil || result.Outcome != calendar.EventSyncObjectStillQuarantined || result.Warning == nil || result.Warning.ETag != `"changed"` {
		t.Fatalf("repair=%#v err=%v", result, err)
	}
}

func TestAppleEventSyncFallsBackToCursorlessReplacementWithInventoryETags(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(inventoryResponse(changedObject("/calendar/a.ics", `"exact-etag"`))))
		case http.MethodGet:
			gets.Add(1)
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte(eventObject("a")))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	p := eventSyncProvider(t, server)
	page, err := p.SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement))
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if !page.Complete || page.NextCursor != "" || gets.Load() != 1 || len(page.Inventory) != 1 || page.Inventory[0].ETag != `"exact-etag"` || len(page.ReplacedObjectIDs) != 1 || page.ReplacedObjectIDs[0] != "/calendar/a.ics" || len(page.Warnings) != 0 {
		t.Fatalf("replacement page = %#v, GETs=%d", page, gets.Load())
	}
	// The frozen request has no previous inventory bridge. A second replacement
	// therefore fetches the same object again rather than pretending an ETag
	// comparison is available.
	if _, err := p.SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement)); err != nil || gets.Load() != 2 {
		t.Fatalf("second safe replacement err=%v GETs=%d, want 2", err, gets.Load())
	}
}

func TestAppleEventSyncInitialReplacementInventoriesWhenEmptyTokenReportHasNoChanges(t *testing.T) {
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			if reports.Add(1) == 1 {
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = w.Write([]byte(syncResponse("current-token", false, "")))
				return
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(queryResponse("/calendar/seed.ics", `"seed-etag"`, eventObject("seed"))))
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement))
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if reports.Load() != 2 || !page.Complete || page.NextCursor != "current-token" || len(page.Upserts) != 1 || len(page.Inventory) != 1 {
		t.Fatalf("initial replacement page=%#v reports=%d", page, reports.Load())
	}
}

func TestAppleEventSyncCalendarQueryMalformedObjectFallsBackToIsolatedFetch(t *testing.T) {
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			if reports.Add(1) == 1 {
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = w.Write([]byte(syncResponse("current-token", false, "")))
				return
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(queryResponse("/calendar/bad.ics", `"bad"`, "BEGIN:VCALENDAR\r\nBROKEN\r\nEND:VCALENDAR\r\n")))
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(inventoryResponse(changedObject("/calendar/bad.ics", `"bad"`))))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nBROKEN\r\nEND:VCALENDAR\r\n"))
		}
	}))
	defer server.Close()

	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement))
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if reports.Load() != 2 || !page.Complete || page.NextCursor != "current-token" || len(page.Warnings) != 1 || page.Warnings[0].Code != calendar.EventSyncProtocol || page.Warnings[0].ObjectID != "/calendar/bad.ics" || page.Warnings[0].Diagnostic == nil || len(page.Warnings[0].Diagnostic.RawPayload) == 0 || len(page.Inventory) != 0 || len(page.ReplacedObjectIDs) != 0 || len(page.DeletedObjectIDs) != 0 {
		t.Fatalf("query fallback page=%#v reports=%d", page, reports.Load())
	}
}

func TestAppleEventSyncFallbackCanonicalizesObjectMissingDuringFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(inventoryResponse(changedObject("/calendar//gone.ics", `"etag"`))))
		case http.MethodGet:
			http.NotFound(w, r)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement))
	if err != nil || !page.Complete || len(page.DeletedObjectIDs) != 1 || page.DeletedObjectIDs[0] != "/calendar/gone.ics" {
		t.Fatalf("page=%#v err=%v", page, err)
	}
}

func TestAppleEventSyncFamilySharingReportFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<broken"))
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(inventoryResponse(changedObject("/calendar/family.ics", `"f"`))))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte(eventObject("family")))
		}
	}))
	defer server.Close()
	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement))
	if err != nil || !page.Complete || len(page.Upserts) != 1 {
		t.Fatalf("Family Sharing fallback = %#v, %v", page, err)
	}
}

func TestAppleEventSyncChangesEveryVEVENTInObjectAndDeletesMissingObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			deleted := `<D:response><D:href>/calendar/gone.ics</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`
			_, _ = w.Write([]byte(syncResponse("new", false, changedObject("/calendar/multi.ics", `"m"`)+deleted)))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:first\r\nDTSTART:20260820T090000Z\r\nDTEND:20260820T100000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:second\r\nDTSTART:20260821T090000Z\r\nDTEND:20260821T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
		}
	}))
	defer server.Close()
	request := eventSyncRequest(calendar.EventSyncIncremental)
	request.Cursor = "old"
	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Upserts) != 2 || len(page.ReplacedObjectIDs) != 1 || page.ReplacedObjectIDs[0] != "/calendar/multi.ics" || len(page.DeletedObjectIDs) != 1 || page.DeletedObjectIDs[0] != "/calendar/gone.ics" {
		t.Fatalf("page = %#v", page)
	}
	if page.Upserts[0].Event.ID == page.Upserts[1].Event.ID {
		t.Fatalf("multi-VEVENT object reused event ID %q", page.Upserts[0].Event.ID)
	}
	if !page.Upserts[0].Event.ReadOnly || !page.Upserts[1].Event.ReadOnly {
		t.Fatalf("multi-VEVENT members must be read-only until member-addressed mutations are supported: %#v", page.Upserts)
	}
}

func TestAppleEventSyncCanonicalizesDeletedObjectIdentity(t *testing.T) {
	p := &Provider{}
	objects, err := p.fetchSyncObjects(context.Background(), eventSyncCalendar, []appleSyncObject{{path: "/calendar//gone.ics", deleted: true}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := appleSyncPage(eventSyncRequest(calendar.EventSyncIncremental), objects)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.DeletedObjectIDs) != 1 || page.DeletedObjectIDs[0] != "/calendar/gone.ics" {
		t.Fatalf("deleted object IDs = %#v", page.DeletedObjectIDs)
	}
}

func TestAppleEventSyncAllMalformedObjectsWarnWithoutMutations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(inventoryResponse(changedObject("/calendar/bad.ics", `"b"`))))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:bad\r\nDTSTART:not-a-date\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
		}
	}))
	defer server.Close()
	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement))
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if !page.Complete || len(page.Warnings) != 1 || page.Warnings[0].Code != calendar.EventSyncProtocol || page.Warnings[0].ObjectID != "/calendar/bad.ics" || page.Warnings[0].Diagnostic == nil || len(page.Warnings[0].Diagnostic.RawPayload) == 0 || len(page.Inventory) != 0 || len(page.ReplacedObjectIDs) != 0 || len(page.DeletedObjectIDs) != 0 {
		t.Fatalf("malformed object page = %#v", page)
	}
}

func TestAppleEventSyncParsedEmptyObjectStillDeletesMembership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(syncResponse("next", false, changedObject("/calendar/empty.ics", `"empty"`))))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
		}
	}))
	defer server.Close()

	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncIncremental))
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if !page.Complete || len(page.DeletedObjectIDs) != 1 || page.DeletedObjectIDs[0] != "/calendar/empty.ics" || len(page.Inventory) != 0 || len(page.ReplacedObjectIDs) != 0 || len(page.Warnings) != 0 {
		t.Fatalf("empty object page = %#v", page)
	}
}

func TestAppleEventSyncIsolatesMalformedObjectsFromValidSiblings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(syncResponse("next", false, changedObject("/calendar/good.ics", `"good"`)+changedObject("/calendar/bad.ics", `"bad"`))))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			if r.URL.Path == "/calendar/good.ics" {
				_, _ = w.Write([]byte(eventObject("good")))
				return
			}
			// A content line without a colon is syntactically invalid iCalendar.
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nBROKEN\r\nEND:VCALENDAR\r\n"))
		}
	}))
	defer server.Close()

	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncIncremental))
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if !page.Complete || page.NextCursor != "next" || len(page.Upserts) != 1 || len(page.Inventory) != 1 || len(page.ReplacedObjectIDs) != 1 || len(page.DeletedObjectIDs) != 0 || len(page.Warnings) != 1 || page.Warnings[0].Code != calendar.EventSyncProtocol || page.Warnings[0].ObjectID != "/calendar/bad.ics" || page.Warnings[0].ETag != `"bad"` || page.Warnings[0].Diagnostic == nil || len(page.Warnings[0].Diagnostic.RawPayload) == 0 {
		t.Fatalf("page = %#v", page)
	}
}

func TestAppleEventSyncTreatsOrphanRecurrenceExceptionAsMalformed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(syncResponse("next", false, changedObject("/calendar/orphan.ics", `"orphan"`))))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:orphan\r\nRECURRENCE-ID:20260820T090000Z\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
		}
	}))
	defer server.Close()

	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncIncremental))
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if !page.Complete || len(page.Warnings) != 1 || page.Warnings[0].Code != calendar.EventSyncProtocol || page.Warnings[0].ObjectID != "/calendar/orphan.ics" || page.Warnings[0].ETag != `"orphan"` || page.Warnings[0].Diagnostic == nil || len(page.Warnings[0].Diagnostic.RawPayload) == 0 || len(page.Inventory) != 0 || len(page.ReplacedObjectIDs) != 0 || len(page.DeletedObjectIDs) != 0 {
		t.Fatalf("orphan exception page = %#v", page)
	}
}

func TestAppleEventSyncFetchFailuresStayHard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantClass  calendar.EventSyncErrorClass
		retryAfter time.Duration
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantClass: calendar.EventSyncAuth},
		{name: "forbidden", status: http.StatusForbidden, wantClass: calendar.EventSyncPermission},
		{name: "rate limited", status: http.StatusTooManyRequests, wantClass: calendar.EventSyncRateLimited, retryAfter: 7 * time.Second},
		{name: "server failure", status: http.StatusBadGateway, wantClass: calendar.EventSyncTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "REPORT":
					w.WriteHeader(http.StatusMultiStatus)
					_, _ = w.Write([]byte(syncResponse("next", false, changedObject("/calendar/object.ics", `"etag"`))))
				case http.MethodGet:
					if tc.status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "7")
					}
					w.WriteHeader(tc.status)
				}
			}))
			defer server.Close()

			_, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncIncremental))
			assertAppleEventSyncError(t, err, tc.wantClass, tc.retryAfter)
		})
	}

	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{name: "missing content type"},
		{name: "wrong content type", contentType: "text/html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "REPORT":
					w.WriteHeader(http.StatusMultiStatus)
					_, _ = w.Write([]byte(syncResponse("next", false, changedObject("/calendar/object.ics", `"etag"`))))
				case http.MethodGet:
					if tc.contentType != "" {
						w.Header().Set("Content-Type", tc.contentType)
					} else {
						w.Header()["Content-Type"] = nil
					}
					_, _ = w.Write([]byte(eventObject("valid")))
				}
			}))
			defer server.Close()

			page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncIncremental))
			if err != nil || len(page.Warnings) != 1 || page.Warnings[0].Diagnostic == nil {
				t.Fatalf("invalid media type should quarantine one object with diagnostics: page=%#v err=%v", page, err)
			}
		})
	}

	t.Run("network", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		p := &Provider{httpClient: &http.Client{Timeout: time.Second}, caldavURL: url}
		_, err := p.SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncIncremental))
		assertAppleEventSyncError(t, err, calendar.EventSyncTransient, 0)
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &Provider{httpClient: http.DefaultClient, caldavURL: "http://127.0.0.1:1"}
		_, err := p.SyncEvents(ctx, eventSyncRequest(calendar.EventSyncIncremental))
		assertAppleEventSyncError(t, err, calendar.EventSyncTransient, 0)
	})
}

func assertAppleEventSyncError(t *testing.T, err error, wantClass calendar.EventSyncErrorClass, wantRetryAfter time.Duration) {
	t.Helper()
	var syncErr *calendar.EventSyncError
	if !errors.As(err, &syncErr) || syncErr == nil || syncErr.Class != wantClass || syncErr.RetryAfter != wantRetryAfter {
		t.Fatalf("error = %#v, want class=%q retry_after=%s", err, wantClass, wantRetryAfter)
	}
}

func TestAppleEventSync403ValidSyncTokenResetsWithoutLeakingDetails(t *testing.T) {
	const staleToken = "stale-sync-token"
	const privatePath = "/calendar/private-resource.ics"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "REPORT" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		// Prefixes are intentionally arbitrary; the adapter matches expanded
		// XML namespace names, not a literal D: prefix.
		_, _ = w.Write([]byte(`<x:error xmlns:x="DAV:"><x:valid-sync-token/><x:href>` + privatePath + `</x:href></x:error>`))
	}))
	defer server.Close()
	request := eventSyncRequest(calendar.EventSyncIncremental)
	request.Cursor = staleToken
	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), request)
	if err != nil {
		t.Fatalf("SyncEvents() error = %v", err)
	}
	if !page.ResetRequired || page.Complete || page.NextCursor != "" || page.NextPageToken != "" || len(page.Upserts) != 0 || len(page.DeletedObjectIDs) != 0 || len(page.Inventory) != 0 {
		t.Fatalf("reset page = %#v", page)
	}
}

func TestAppleEventSync403PermissionDoesNotResetOrLeakResponse(t *testing.T) {
	const staleToken = "permission-token"
	const privatePath = "/calendar/private-resource.ics"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<D:error xmlns:D="DAV:"><D:need-privileges/><D:href>` + privatePath + `</D:href></D:error>`))
	}))
	defer server.Close()
	request := eventSyncRequest(calendar.EventSyncIncremental)
	request.Cursor = staleToken
	page, err := eventSyncProvider(t, server).SyncEvents(context.Background(), request)
	if err == nil {
		t.Fatal("SyncEvents() error = nil, want permission failure")
	}
	if page.ResetRequired || !strings.Contains(err.Error(), string(calendar.EventSyncPermission)) || strings.Contains(err.Error(), staleToken) || strings.Contains(err.Error(), privatePath) || strings.Contains(err.Error(), "need-privileges") {
		t.Fatalf("page=%#v error leaked response or was wrong class: %v", page, err)
	}
}

func TestAppleEventSyncPolicy(t *testing.T) {
	policy := (&Provider{}).EventSyncPolicy()
	if policy.PollInterval != 5*time.Minute || policy.RetryBase != time.Minute || policy.RetryMax != 15*time.Minute || policy.MaxPages != 250 || policy.MaxResets != 2 {
		t.Fatalf("EventSyncPolicy() = %#v", policy)
	}
}
