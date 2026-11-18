package apple

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"calendar-mcp/internal/calendar"
)

// These local fixtures document observable classifications, not the cause of
// any historical provider response. New uses the production HTTP transport.
func TestEventSyncPermissionDiagnosticFixtures(t *testing.T) {
	const forbiddenEntry = `<D:response><D:href>/calendar/denied.ics</D:href><D:propstat><D:prop><D:getetag/></D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat></D:response>`
	for _, tc := range []struct {
		name         string
		reportStatus int
		reportBody   string
		getStatus    int
		wantClass    calendar.EventSyncErrorClass
		wantReset    bool
		wantUpserts  int
	}{
		{name: "top-level permission", reportStatus: 403, reportBody: `<D:error xmlns:D="DAV:"><D:need-privileges/></D:error>`, wantClass: calendar.EventSyncPermission},
		{name: "expired sync token", reportStatus: 403, reportBody: `<D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`, wantReset: true},
		{name: "normalized top-level 403 with inner propstat 403", reportStatus: 403, reportBody: syncResponse("next", false, forbiddenEntry), wantReset: true},
		{name: "multistatus inner propstat 403", reportStatus: 207, reportBody: syncResponse("next", false, forbiddenEntry), wantReset: true},
		{name: "object GET permission", reportStatus: 207, reportBody: syncResponse("next", false, changedObject("/calendar/denied.ics", `"e"`)), getStatus: 403, wantClass: calendar.EventSyncPermission},
		{name: "healthy sibling", reportStatus: 207, reportBody: syncResponse("next", false, changedObject("/calendar/healthy.ics", `"e"`)), getStatus: 200, wantUpserts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var getCount int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "REPORT":
					w.WriteHeader(tc.reportStatus)
					fmt.Fprint(w, tc.reportBody)
				case http.MethodGet:
					getCount++
					if tc.getStatus == 200 {
						w.Header().Set("Content-Type", "text/calendar")
					}
					w.WriteHeader(tc.getStatus)
					if tc.getStatus == 200 {
						fmt.Fprint(w, eventObject("healthy"))
					}
				default:
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer server.Close()
			p, err := New("fixture", "fixture", server.URL)
			if err != nil {
				t.Fatal(err)
			}
			req := eventSyncRequest(calendar.EventSyncIncremental)
			req.Cursor = "synthetic-cursor"
			page, err := p.SyncEvents(context.Background(), req)
			if tc.wantClass != "" {
				assertAppleEventSyncError(t, err, tc.wantClass, 0)
			} else if err != nil {
				t.Fatalf("SyncEvents: %v", err)
			}
			if page.ResetRequired != tc.wantReset || len(page.Upserts) != tc.wantUpserts {
				t.Fatalf("reset=%v upserts=%d", page.ResetRequired, len(page.Upserts))
			}
			if tc.wantUpserts > 0 && !page.Complete {
				t.Fatal("healthy response was not complete")
			}
			if (tc.getStatus != 0) != (getCount == 1) {
				t.Fatalf("GET count = %d", getCount)
			}
		})
	}
}
