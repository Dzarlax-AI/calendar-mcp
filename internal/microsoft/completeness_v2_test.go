package microsoft

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestV2EventPageRequiresValueArray(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{"missing", `{}`, true}, {"null", `{"value":null}`, true},
		{"wrong type", `{"value":{}}`, true}, {"empty", `{"value":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			p := &Provider{client: srv.Client(), baseURL: srv.URL}
			now := time.Now().UTC()
			page, err := p.ListEventsV2(context.Background(), calendar.ListEventsRequestV2{CalendarID: "synthetic", Start: now, End: now.Add(time.Hour), View: calendar.RecurrenceExpanded})
			if (err != nil) != tc.wantErr || (!tc.wantErr && !page.Complete) {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			_, err = p.GetEventInstancesV2(context.Background(), calendar.InstancesRequestV2{Ref: calendar.EventRef{CalendarID: "synthetic", EventID: "synthetic"}, Start: now, End: now.Add(time.Hour)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("instances err=%v", err)
			}
		})
	}
}

func TestV2BothRejectsMalformedNextPageAndPaginationLoop(t *testing.T) {
	for _, tc := range []struct {
		name string
		loop bool
	}{{"missing next page", false}, {"repeated link", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "2" && !tc.loop {
					fmt.Fprint(w, `{}`)
					return
				}
				fmt.Fprintf(w, `{"value":[],"@odata.nextLink":%q}`, srv.URL+"/next?page=2")
			}))
			defer srv.Close()
			p := &Provider{client: srv.Client(), baseURL: srv.URL}
			now := time.Now().UTC()
			page, err := p.ListEventsV2(context.Background(), calendar.ListEventsRequestV2{CalendarID: "synthetic", Start: now, End: now.Add(time.Hour), View: calendar.RecurrenceBoth})
			if err == nil {
				t.Fatalf("malformed pagination accepted: %+v", page)
			}
		})
	}
}
