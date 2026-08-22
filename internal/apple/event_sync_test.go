package apple

import (
	"context"
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
	if !page.Complete || page.NextCursor != "" || gets.Load() != 1 || len(page.Inventory) != 1 || page.Inventory[0].ETag != `"exact-etag"` || len(page.ReplacedObjectIDs) != 1 || page.ReplacedObjectIDs[0] != "/calendar/a.ics" {
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

func TestAppleEventSyncMalformedObjectBlocksReplacementCompletion(t *testing.T) {
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
	_, err := eventSyncProvider(t, server).SyncEvents(context.Background(), eventSyncRequest(calendar.EventSyncReplacement))
	if err == nil || !strings.Contains(err.Error(), string(calendar.EventSyncProtocol)) || strings.Contains(err.Error(), "bad.ics") {
		t.Fatalf("malformed object error = %v", err)
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
