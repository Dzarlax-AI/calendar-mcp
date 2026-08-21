package apple

import (
	"context"
	"net/http"
	"net/http/httptest"
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
