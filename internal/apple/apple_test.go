package apple

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

func TestSetAppleEventTime_AllDayUsesDate(t *testing.T) {
	ev := ical.NewEvent()
	tm := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)

	setAppleEventTime(*ev, ical.PropDateTimeStart, tm, true)

	prop := ev.Props.Get(ical.PropDateTimeStart)
	if prop == nil {
		t.Fatalf("DTSTART property was not set")
	}
	if got := prop.ValueType(); got != ical.ValueDate {
		t.Fatalf("ValueType = %q, want %q", got, ical.ValueDate)
	}
}

func TestGetEventsFallbackReturnsPropfindFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	p := &Provider{httpClient: server.Client(), caldavURL: server.URL}

	_, err := p.getEventsFallback(context.Background(), "/calendar/", time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("getEventsFallback() returned nil error for PROPFIND failure")
	}
}

func TestCalendarObjectsFallbackSkipsObjectsDeletedAfterPropfind(t *testing.T) {
	const calendarPath = "/calendar/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PROPFIND" && r.URL.Path == calendarPath:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/calendar/active.ics</D:href></D:response><D:response><D:href>/calendar/deleted.ics</D:href></D:response></D:multistatus>`))
		case r.Method == http.MethodGet && r.URL.Path == "/calendar/active.ics":
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:active\r\nDTSTART:20260821T090000Z\r\nDTEND:20260821T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/calendar/deleted.ics":
			http.NotFound(w, r)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := caldav.NewClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{client: client, httpClient: server.Client(), caldavURL: server.URL}

	objects, err := p.getCalendarObjectsFallback(t.Context(), calendarPath)
	if err != nil {
		t.Fatalf("getCalendarObjectsFallback() error = %v", err)
	}
	if len(objects) != 1 || objects[0].Path != "/calendar/active.ics" {
		t.Fatalf("objects = %#v, want only active object", objects)
	}
	if _, err := p.getCalendarObjectsFallbackWithCompleteness(t.Context(), calendarPath, true); err == nil {
		t.Fatal("strict V2 fallback accepted an object disappearing after PROPFIND")
	}
}

func TestConvertEventMarksAllDay(t *testing.T) {
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, "event-1")
	ev.Props.SetText(ical.PropSummary, "All day")
	ev.Props.SetDate(ical.PropDateTimeStart, time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC))
	ev.Props.SetDate(ical.PropDateTimeEnd, time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC))

	got := convertEvent(*ev, "cal", "cal/event-1.ics")
	if !got.AllDay {
		t.Fatalf("AllDay = false, want true")
	}
}

func TestCreateEventResponsePreservesAllDay(t *testing.T) {
	got := newCreatedEvent("cal", "event-1", calendar.EventCreate{
		Title:  "All day",
		Start:  time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
		End:    time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		AllDay: true,
	})
	if !got.AllDay {
		t.Fatalf("AllDay = false, want true")
	}
}

func TestAppleObjectPathPreservesCalDAVHref(t *testing.T) {
	const href = "/123/calendars/family/resource-name.ics"

	got, err := appleObjectPath("/123/calendars/family/", href)
	if err != nil {
		t.Fatal(err)
	}
	if got != href {
		t.Fatalf("appleObjectPath() = %q, want %q", got, href)
	}
}

func TestAppleObjectPathSupportsLegacyUID(t *testing.T) {
	const calendarPath = "/123/calendars/family/"

	got, err := appleObjectPath(calendarPath, "legacy-uid")
	if err != nil {
		t.Fatal(err)
	}
	if got != calendarPath+"legacy-uid.ics" {
		t.Fatalf("appleObjectPath() = %q, want %q", got, calendarPath+"legacy-uid.ics")
	}
}

func TestAppleObjectPathRejectsCrossCalendarHref(t *testing.T) {
	if _, err := appleObjectPath("/123/calendars/family/", "/123/calendars/private/event.ics"); err == nil {
		t.Fatal("appleObjectPath() accepted an href outside the requested calendar")
	}
}

func TestConvertEventUsesResourceHrefAsID(t *testing.T) {
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, "ical-uid")
	ev.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC))
	ev.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC))
	const href = "/123/calendars/family/resource-name.ics"

	got := convertEvent(*ev, "/123/calendars/family/", href)

	if got.ID != href {
		t.Fatalf("ID = %q, want href %q", got.ID, href)
	}
}

func TestAppleV2MapsRecurrenceAndHref(t *testing.T) {
	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, "uid")
	event.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC))
	event.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC))
	rule := ical.NewProp(ical.PropRecurrenceRule)
	rule.Value = "FREQ=WEEKLY;BYDAY=MO"
	event.Props.Add(rule)
	result := appleEventV2(*event, "/calendar/", "/calendar/object.ics", `"etag"`)
	if result.ID != "/calendar/object.ics" || result.ICalUID != "uid" || len(result.Recurrence) != 1 || result.Recurrence[0] != "RRULE:FREQ=WEEKLY;BYDAY=MO" {
		t.Fatalf("appleEventV2() = %#v", result)
	}
}

func TestAppleV2PreservesRecurrenceParameters(t *testing.T) {
	event := ical.NewEvent()
	date := ical.NewProp(ical.PropExceptionDates)
	date.Params.Set("TZID", "Europe/Belgrade")
	date.Value = "20260817T090000"
	event.Props.Add(date)

	result := appleEventV2(*event, "/calendar/", "/calendar/object.ics", "etag")
	if len(result.Recurrence) != 1 || result.Recurrence[0] != "EXDATE;TZID=Europe/Belgrade:20260817T090000" {
		t.Fatalf("recurrence = %#v", result.Recurrence)
	}
	restored := ical.NewEvent()
	if err := setAppleRecurrence(restored, result.Recurrence); err != nil {
		t.Fatal(err)
	}
	if got := restored.Props.Get(ical.PropExceptionDates).Params.Get("TZID"); got != "Europe/Belgrade" {
		t.Fatalf("TZID = %q", got)
	}
}

func TestAppleV2RejectsAttendeeWrites(t *testing.T) {
	err := validateAppleWrite(calendar.EventCreateV2{Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "person@example.com"}}}})
	if err == nil {
		t.Fatal("validateAppleWrite() allowed an attendee write")
	}
}

func TestAvailabilityOnlyMakesDisappearedFallbackStrict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<D:multistatus xmlns:D="DAV:">`))
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/calendar/deleted.ics</D:href></D:response></D:multistatus>`))
		case "GET":
			http.NotFound(w, r)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := caldav.NewClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{client: client, httpClient: server.Client(), caldavURL: server.URL}
	req := calendar.ListEventsRequestV2{CalendarID: "/calendar/", Start: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), View: calendar.RecurrenceExpanded}
	page, err := p.ListEventsV2(t.Context(), req)
	if err != nil || !page.Complete {
		t.Fatalf("ordinary list changed: %v", err)
	}
	req.AvailabilityStrict = true
	if _, err = p.ListEventsV2(t.Context(), req); err == nil {
		t.Fatal("strict availability accepted missing object")
	}
}

func TestGatewayAppleReportFallbackPreservesEscapedDAVObjects(t *testing.T) {
	const collection = "/calendar/"
	var requests, reports atomic.Int32
	start := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	p, err := New("fixture", "fixture", "https://fixture.invalid")
	if err != nil {
		t.Fatal(err)
	}
	p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
		const open = `<D:multistatus xmlns:D="DAV:">`
		const closeTag = `</D:multistatus>`
		status, media, body := http.StatusMultiStatus, "application/xml", open+closeTag
		switch r.Method {
		case "REPORT":
			reports.Add(1)
			status, body = http.StatusInternalServerError, "<D:error"
		case "PROPFIND":
			var entries strings.Builder
			entries.WriteString(open)
			for i := range 254 {
				href := fmt.Sprintf("%sobject-%d.ics", collection, i)
				propStatus := 200
				if i < 8 {
					href = fmt.Sprintf("%sobject-%d%%40fixture.ics", collection, i)
				}
				if i == 8 {
					propStatus = 404
				}
				fmt.Fprintf(&entries, `<D:response><D:href>%s</D:href><D:propstat><D:prop><D:getetag/></D:prop><D:status>HTTP/1.1 %d status</D:status></D:propstat></D:response>`, href, propStatus)
			}
			entries.WriteString(`<D:response><D:href>https://fixture.invalid/calendar/object-253.ics</D:href></D:response>` + closeTag)
			body = entries.String()
		case "GET":
			requests.Add(1)
			if strings.Contains(r.URL.Path, "@") && !strings.Contains(r.URL.EscapedPath(), "%40") {
				t.Errorf("encoded basename was not preserved: %s", r.URL.EscapedPath())
				status = http.StatusNotFound
				break
			}
			if strings.Contains(r.URL.EscapedPath(), "%2540") {
				status = http.StatusNotFound
				break
			}
			status, media = http.StatusOK, "text/calendar"
			body = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + r.URL.Path + "\r\nDTSTART:20260821T090000Z\r\nDTEND:20260821T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header: http.Header{"Content-Type": {media}, "Etag": {`"fixture"`}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	page, err := p.ListEventsV2(t.Context(), calendar.ListEventsRequestV2{CalendarID: collection,
		Start: start, End: start.AddDate(0, 0, 1), View: calendar.RecurrenceBoth, AvailabilityStrict: true})
	if err != nil || !page.Complete || len(page.Items) != 254 || requests.Load() != 254 || reports.Load() != 1 {
		t.Fatalf("items=%d complete=%t GET=%d REPORT=%d err=%v", len(page.Items), page.Complete, requests.Load(), reports.Load(), err)
	}
	found := false
	for _, event := range page.Items {
		if event.ID == "/calendar/object-0@fixture.ics" && event.ETag == "fixture" {
			found = true
		}
	}
	if !found {
		t.Fatal("fallback changed the canonical resource ID or ETag")
	}
}

func TestAppleStrictFallbackRejectsInvalidInventoryBeforeGET(t *testing.T) {
	for _, tc := range []struct{ name, href, status string }{
		{"foreign origin", "https://foreign.invalid/calendar/blocked.ics", ""},
		{"encoded slash", "/calendar%2Fblocked.ics", ""},
		{"encoded traversal", "/calendar/%2e%2e/calendar/blocked.ics", ""},
		{"query", "/calendar/blocked.ics?ref=other", ""},
		{"object 404", "/calendar/blocked.ics", "<D:status>HTTP/1.1 404 Not Found</D:status>"},
		{"property 403", "/calendar/blocked.ics", "<D:propstat><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New("fixture", "fixture", "https://fixture.invalid")
			if err != nil {
				t.Fatal(err)
			}
			p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != "PROPFIND" {
					t.Errorf("invalid inventory started %s", r.Method)
				}
				body := `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/calendar/first.ics</D:href></D:response><D:response><D:href>` + tc.href + "</D:href>" + tc.status + "</D:response></D:multistatus>"
				return &http.Response{StatusCode: 207, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			if _, err := p.getCalendarObjectsFallbackWithCompleteness(t.Context(), "/calendar/", true); err == nil {
				t.Fatal("invalid inventory was accepted")
			}
		})
	}
}

func TestAppleFallbackDoesNotFollowRedirect(t *testing.T) {
	p, err := New("fixture", "fixture", "https://fixture.invalid")
	if err != nil {
		t.Fatal(err)
	}
	p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "fixture.invalid" {
			t.Errorf("unexpected destination %s", r.URL.Host)
		}
		if r.Method == "PROPFIND" {
			body := `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/calendar/redirect.ics</D:href></D:response></D:multistatus>`
			return &http.Response{StatusCode: 207, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://other.invalid/calendar/redirect.ics"}},
			Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	if _, err := p.getCalendarObjectsFallbackWithCompleteness(t.Context(), "/calendar/", true); err == nil {
		t.Fatal("redirect was accepted")
	}
}

func TestAppleStrictFallbackChecksEmptyCollectionStatus(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		wantError    bool
	}{
		{"successful empty collection", "<D:status>HTTP/1.1 200 OK</D:status>", false},
		{"failed collection", "<D:status>HTTP/1.1 500 Internal Server Error</D:status>", true},
		{"failed collection property", "<D:propstat><D:status>HTTP/1.1 500 Internal Server Error</D:status></D:propstat>", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const collection = "/calendar/"
			p, err := New("fixture", "fixture", "https://fixture.invalid")
			if err != nil {
				t.Fatal(err)
			}
			p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusInternalServerError, "<D:error"
				switch r.Method {
				case "REPORT":
				case "PROPFIND":
					status = http.StatusMultiStatus
					body = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>` + collection + "</D:href>" + tc.status + "</D:response></D:multistatus>"
				default:
					t.Errorf("unexpected request: %s", r.Method)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			start := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
			page, err := p.ListEventsV2(t.Context(), calendar.ListEventsRequestV2{
				CalendarID: collection, Start: start, End: start.Add(time.Hour),
				View: calendar.RecurrenceBoth, AvailabilityStrict: true,
			})
			if (err != nil) != tc.wantError || page.Complete == tc.wantError || len(page.Items) != 0 {
				t.Fatalf("empty listing: complete=%t items=%d error=%v, want error=%t", page.Complete, len(page.Items), err, tc.wantError)
			}
		})
	}
}
