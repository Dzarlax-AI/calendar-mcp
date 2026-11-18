package microsoft

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestGatewayCancellationInventoryFindsSeriesWithNoRemainingInstances(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected provider mutation %s", r.Method)
		}
		switch r.URL.Path {
		case "/me/calendars/fixture/calendarView":
			fmt.Fprint(w, `{"value":[]}`)
		case "/me/calendars/fixture/events":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"value":[{"id":"unknown","type":"seriesMaster"},{"id":"no-cancellations","type":"seriesMaster"},{"id":"single","type":"singleInstance"}]}`)
			} else {
				fmt.Fprintf(w, `{"value":[{"id":"all-cancelled","type":"seriesMaster"}],"@odata.nextLink":%q}`, server.URL+"/me/calendars/fixture/events?page=2")
			}
		case "/me/calendars/fixture/events/all-cancelled", "/me/calendars/fixture/events/unknown", "/me/calendars/fixture/events/no-cancellations":
			if !strings.Contains(r.URL.Query().Get("$select"), "cancelledOccurrences") {
				t.Error("cancellation receipt not requested")
			}
			id := strings.TrimPrefix(r.URL.Path, "/me/calendars/fixture/events/")
			cancellations := ""
			switch id {
			case "all-cancelled":
				cancellations = `,"cancelledOccurrences":["OID.all-cancelled.2026-10-25","OID.all-cancelled.2026-10-26"]`
			case "no-cancellations":
				cancellations = `,"cancelledOccurrences":[]`
			}
			fmt.Fprintf(w, `{"id":%q,"type":"seriesMaster","sensitivity":"normal","subject":"fixture","start":{"dateTime":"2026-10-24T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-24T10:00:00","timeZone":"UTC"},"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"2026-10-24","recurrenceTimeZone":"W. Europe Standard Time"}}%s}`, id, cancellations)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &Provider{baseURL: server.URL, client: server.Client()}
	page, err := p.ListEventsV2(t.Context(), calendar.ListEventsRequestV2{CalendarID: "fixture", Start: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), View: calendar.RecurrenceBoth, ShowDeleted: true, GatewayCancellationInventory: true})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete || !maps.Equal(page.VerifiedCancellationSeries, map[string]string{"all-cancelled": "Europe/Berlin", "no-cancellations": "Europe/Berlin"}) {
		t.Fatalf("invalid cancellation scope: %#v", page.VerifiedCancellationSeries)
	}
	if len(page.Items) != 5 {
		t.Fatalf("expected three masters and two cancellations: %#v", page.Items)
	}
	want := map[string]string{"OID.all-cancelled.2026-10-25": "2026-10-25T11:00:00+01:00", "OID.all-cancelled.2026-10-26": "2026-10-26T11:00:00+01:00"}
	for _, event := range page.Items {
		if value, ok := want[event.ID]; ok {
			if event.OriginalStart == nil || event.OriginalStart.DateTime != value || event.Visibility != "normal" || event.Status != "cancelled" {
				t.Fatalf("wrong cancellation: %#v", event)
			}
			delete(want, event.ID)
		}
	}
	if len(want) != 0 {
		t.Fatal("cancelled occurrences not materialized")
	}
}

func TestGatewayCancellationInventoryFailureKeepsOrdinaryView(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprint(stall), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected provider mutation %s", r.Method)
				}
				switch r.URL.Path {
				case "/me/calendars/fixture/calendarView":
					fmt.Fprint(w, `{"value":[{"id":"instance","type":"occurrence","seriesMasterId":"master","occurrenceId":"OID.master.2026-10-25","sensitivity":"normal","subject":"ordinary","start":{"dateTime":"2026-10-25T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-25T10:00:00","timeZone":"UTC"}}]}`)
				case "/me/calendars/fixture/events/master":
					fmt.Fprint(w, `{"id":"master","type":"seriesMaster","sensitivity":"normal","subject":"master","start":{"dateTime":"2026-10-24T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-24T10:00:00","timeZone":"UTC"},"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"2026-10-24","recurrenceTimeZone":"UTC"}},"cancelledOccurrences":[]}`)
				case "/me/calendars/fixture/events":
					if stall {
						<-r.Context().Done()
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			p := &Provider{baseURL: server.URL, client: server.Client()}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			page, err := p.ListEventsV2(ctx, calendar.ListEventsRequestV2{CalendarID: "fixture", Start: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
				End: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), View: calendar.RecurrenceBoth, ShowDeleted: true, GatewayCancellationInventory: true})
			if err != nil || !page.Complete || len(page.Items) != 2 || page.Items[0].ID != "master" || page.Items[1].ID != "instance" {
				t.Fatalf("ordinary snapshot was lost: %+v error=%v", page, err)
			}
			if !maps.Equal(page.VerifiedCancellationSeries, map[string]string{"master": "UTC"}) || ctx.Err() != nil {
				t.Fatalf("scope=%+v context error=%v", page.VerifiedCancellationSeries, ctx.Err())
			}
		})
	}
}
