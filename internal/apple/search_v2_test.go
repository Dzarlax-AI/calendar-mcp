package apple

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"github.com/emersion/go-webdav/caldav"
	"regexp"
)

const searchICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
	"BEGIN:VEVENT\r\nUID:series\r\nSUMMARY:JetBrains series\r\nDTSTART;TZID=Europe/Belgrade:20261024T100000\r\nDTEND;TZID=Europe/Belgrade:20261024T110000\r\nRRULE:FREQ=DAILY;COUNT=3\r\nEXDATE;TZID=Europe/Belgrade:20261026T100000\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:series\r\nRECURRENCE-ID;TZID=Europe/Belgrade:20261025T100000\r\nSUMMARY:Moved elsewhere\r\nLOCATION:JetBrains office\r\nDTSTART;TZID=Europe/Belgrade:20261025T120000\r\nDTEND;TZID=Europe/Belgrade:20261025T130000\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:series\r\nRECURRENCE-ID;TZID=Europe/Belgrade:20261026T100000\r\nSUMMARY:JetBrains cancelled\r\nSTATUS:CANCELLED\r\nDTSTART;TZID=Europe/Belgrade:20261026T100000\r\nDTEND;TZID=Europe/Belgrade:20261026T110000\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:allday\r\nSUMMARY:КОНФЕРЕНЦИЯ\r\nDESCRIPTION:JetBrains all day\r\nDTSTART;VALUE=DATE:20261023\r\nDTEND;VALUE=DATE:20261024\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func newSearchFixture(t *testing.T, ics string, status int) (*Provider, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "REPORT" {
			t.Errorf("unexpected calendar mutation/request %s", r.Method)
			http.Error(w, "unexpected", 400)
			return
		}
		if status != 207 {
			http.Error(w, "fixture failure", status)
			return
		}

		groups := map[string]string{}
		for _, event := range regexp.MustCompile(`(?s)BEGIN:VEVENT\r\n.*?END:VEVENT\r\n`).FindAllString(ics, -1) {
			uid := strings.SplitN(strings.SplitN(event, "UID:", 2)[1], "\r\n", 2)[0]
			groups[uid] += event
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		_, _ = fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
		for uid, data := range groups {
			var escaped strings.Builder
			_ = xml.EscapeText(&escaped, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n"+data+"END:VCALENDAR\r\n"))
			_, _ = fmt.Fprintf(w, `<D:response><D:href>/cal/%s.ics</D:href><D:propstat><D:prop><D:getetag>"fixture"</D:getetag><C:calendar-data>%s</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, uid, escaped.String())
		}
		_, _ = fmt.Fprint(w, `</D:multistatus>`)
	}))
	t.Cleanup(server.Close)
	client, err := caldav.NewClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Provider{client: client, httpClient: server.Client(), caldavURL: server.URL}, calls
}
func searchRequest() calendar.SearchEventsRequestV2 {
	return calendar.SearchEventsRequestV2{CalendarID: "/cal/", Query: "jetbrains", Start: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), MaxResults: 1}
}
func searcher(t *testing.T, p *Provider) calendar.SearchProviderV2 {
	t.Helper()
	s, ok := any(p).(calendar.SearchProviderV2)
	if !ok {
		t.Fatal("Apple does not implement SearchProviderV2")
	}
	return s
}
func TestAppleSearchExpandedPagination(t *testing.T) {
	p, _ := newSearchFixture(t, searchICS, 207)
	s := searcher(t, p)
	req := searchRequest()
	var items []calendar.EventV2
	seen := map[string]bool{}
	for n := 0; n < 5; n++ {
		page, err := s.SearchEventsV2(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !page.Complete || len(page.Items) > 1 {
			t.Fatalf("invalid page %#v", page)
		}
		items = append(items, page.Items...)
		if page.NextPageToken == "" {
			break
		}
		if seen[page.NextPageToken] {
			t.Fatal("repeated token")
		}
		seen[page.NextPageToken] = true
		req.PageToken = page.NextPageToken
	}
	if len(items) != 3 || items[0].Start.Date != "2026-10-23" || items[1].Start.DateTime != "2026-10-24T10:00:00+02:00" || items[2].Start.DateTime != "2026-10-25T12:00:00+01:00" {
		t.Fatalf("expanded matches %#v", items)
	}
	caps, err := p.Capabilities(t.Context(), req.CalendarID)
	if err != nil || !caps.Operations.Search || caps.Reasons["search"] == "" {
		t.Fatalf("capabilities %#v %v", caps, err)
	}
}
func TestAppleSearchFilters(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		types       []string
		deleted     bool
		want        int
	}{
		{name: "unicode", query: "конференция", want: 1}, {name: "missing", query: "not found", want: 0},
		{name: "location", query: "office", want: 1}, {name: "description", query: "all day", want: 1},
		{name: "types", query: "jetbrains", types: []string{"birthday"}, want: 0},
		{name: "default type", query: "jetbrains", types: []string{"birthday", "default"}, want: 3},
		{name: "cancelled", query: "jetbrains", deleted: true, want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newSearchFixture(t, searchICS, 207)
			req := searchRequest()
			req.Query = tc.query
			req.EventTypes = tc.types
			req.ShowDeleted = tc.deleted
			req.MaxResults = 50
			page, err := searcher(t, p).SearchEventsV2(t.Context(), req)
			if err != nil || !page.Complete || len(page.Items) != tc.want {
				t.Fatalf("page=%#v err=%v", page, err)
			}
		})
	}
}
func TestAppleSearchRejectsInvalidRequestBeforeRead(t *testing.T) {
	mutations := map[string]func(*calendar.SearchEventsRequestV2){
		"unbounded":      func(r *calendar.SearchEventsRequestV2) { r.Start = time.Time{}; r.End = time.Time{} },
		"half range":     func(r *calendar.SearchEventsRequestV2) { r.End = time.Time{} },
		"reverse range":  func(r *calendar.SearchEventsRequestV2) { r.End = r.Start },
		"empty query":    func(r *calendar.SearchEventsRequestV2) { r.Query = "" },
		"bad token":      func(r *calendar.SearchEventsRequestV2) { r.PageToken = "not-json" },
		"negative limit": func(r *calendar.SearchEventsRequestV2) { r.MaxResults = -1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, calls := newSearchFixture(t, searchICS, 207)
			req := searchRequest()
			mutate(&req)
			_, err := searcher(t, p).SearchEventsV2(t.Context(), req)
			var apiErr *calendar.APIError
			if !errors.As(err, &apiErr) || calls.Load() != 0 || apiErr.Retryable {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
		})
	}
}
func TestAppleSearchBindsTokenToRequest(t *testing.T) {
	p, calls := newSearchFixture(t, searchICS, 207)
	s := searcher(t, p)
	req := searchRequest()
	page, err := s.SearchEventsV2(t.Context(), req)
	if err != nil || page.NextPageToken == "" {
		t.Fatalf("%#v %v", page, err)
	}
	for name, mutate := range map[string]func(*calendar.SearchEventsRequestV2){
		"calendar": func(r *calendar.SearchEventsRequestV2) { r.CalendarID = "/other/" }, "query": func(r *calendar.SearchEventsRequestV2) { r.Query = "office" },
		"range": func(r *calendar.SearchEventsRequestV2) { r.End = r.End.Add(time.Hour) }, "deleted": func(r *calendar.SearchEventsRequestV2) { r.ShowDeleted = true },
		"types": func(r *calendar.SearchEventsRequestV2) { r.EventTypes = []string{"default"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := req
			changed.PageToken = page.NextPageToken
			mutate(&changed)
			before := calls.Load()
			_, err := s.SearchEventsV2(t.Context(), changed)
			var apiErr *calendar.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != calendar.ErrorInvalidArgument || calls.Load() != before {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
		})
	}
}
func TestAppleSearchPropagatesReadFailureAndCancellation(t *testing.T) {
	p, calls := newSearchFixture(t, "", http.StatusServiceUnavailable)
	s := searcher(t, p)
	if _, err := s.SearchEventsV2(t.Context(), searchRequest()); err == nil {
		t.Fatal("provider failure hidden")
	}
	before := calls.Load()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.SearchEventsV2(ctx, searchRequest()); !errors.Is(err, context.Canceled) || calls.Load() != before {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestAppleSearchDoesNotClaimCompleteForMalformedObjects(t *testing.T) {
	malformed := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:bad\r\nSUMMARY:JetBrains\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	p, _ := newSearchFixture(t, malformed, 207)
	page, err := searcher(t, p).SearchEventsV2(t.Context(), searchRequest())
	if err == nil || page.Complete {
		t.Fatalf("malformed data was hidden: %#v %v", page, err)
	}
}

func TestAppleSearchUsesHalfOpenWindowAndDefaultLimit(t *testing.T) {
	p, _ := newSearchFixture(t, searchICS, 207)
	req := searchRequest()
	req.Start = time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	req.End = time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)
	req.MaxResults = 0
	page, err := searcher(t, p).SearchEventsV2(t.Context(), req)
	if err != nil || len(page.Items) != 1 || page.Items[0].Title != "JetBrains series" || page.NextPageToken != "" {
		t.Fatalf("%#v %v", page, err)
	}
}

func TestAppleSearchAcceptsImplicitEnds(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         int
		end          string
	}{
		{"all day", "DTSTART;VALUE=DATE:20261001\r\n", 1, "2026-10-02"},
		{"point at window start", "DTSTART:20261001T000000Z\r\n", 1, "2026-10-01T00:00:00Z"},
		{"duration", "DTSTART:20261001T000000Z\r\nDURATION:PT90M\r\n", 1, "2026-10-01T01:30:00Z"},
		{"point at window end", "DTSTART:20261101T000000Z\r\n", 0, ""},
		{"recurring point", "DTSTART:20261001T000000Z\r\nRRULE:FREQ=DAILY;COUNT=2\r\n", 2, "2026-10-01T00:00:00Z"},
		{"recurring all day", "DTSTART;VALUE=DATE:20261001\r\nRRULE:FREQ=DAILY;COUNT=2\r\n", 2, "2026-10-02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:implicit\r\nSUMMARY:JetBrains\r\n" + tc.fields + "END:VEVENT\r\nEND:VCALENDAR\r\n"
			p, _ := newSearchFixture(t, ics, 207)
			req := searchRequest()
			req.MaxResults = 50
			page, err := searcher(t, p).SearchEventsV2(t.Context(), req)
			if err != nil || !page.Complete || len(page.Items) != tc.want {
				t.Fatalf("page %#v err=%v", page, err)
			}
			if tc.want > 0 && page.Items[0].End.Date+page.Items[0].End.DateTime != tc.end {
				t.Fatalf("end %#v", page.Items[0].End)
			}
		})
	}
}
