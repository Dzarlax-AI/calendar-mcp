package microsoft

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestToGraphDateTimeConvertsInstantToUTC(t *testing.T) {
	input := time.Date(2026, 8, 10, 13, 30, 0, 0, time.FixedZone("CEST", 2*60*60))

	got := toGraphDateTime(input)

	if got.DateTime != "2026-08-10T11:30:00" {
		t.Fatalf("DateTime = %q, want 2026-08-10T11:30:00", got.DateTime)
	}
	if got.TimeZone != "UTC" {
		t.Fatalf("TimeZone = %q, want UTC", got.TimeZone)
	}
}

func TestLegacyAttendeeWritesAreRejectedWithoutNotificationPolicy(t *testing.T) {
	p := &Provider{}
	attendees := []calendar.Attendee{{Email: "person@example.com"}}

	if _, err := p.CreateEvent(context.Background(), "calendar", calendar.EventCreate{
		Title:     "Unsafe",
		Start:     time.Now(),
		End:       time.Now().Add(time.Hour),
		Attendees: attendees,
	}); err == nil {
		t.Fatal("CreateEvent() returned nil error for attendee write without explicit notification policy")
	}

	if _, err := p.UpdateEvent(context.Background(), "calendar", "event", calendar.EventUpdate{
		Attendees: &attendees,
	}); err == nil {
		t.Fatal("UpdateEvent() returned nil error for attendee write without explicit notification policy")
	}
}

func TestListCalendarsFollowsNextLink(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/next-calendars" {
			_, _ = io.WriteString(w, `{"value":[{"id":"second","name":"Second","canEdit":true}]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"value":[{"id":"first","name":"First","canEdit":true}],"@odata.nextLink":%q}`, server.URL+"/next-calendars")
	}))
	defer server.Close()
	p := &Provider{client: server.Client(), baseURL: server.URL}

	got, err := p.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("ListCalendars() = %#v, want both pages", got)
	}
}

func TestGetEventsFollowsNextLink(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/next-events" {
			_, _ = io.WriteString(w, `{"value":[{"id":"second","subject":"Second","start":{"dateTime":"2026-08-10T11:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-10T12:00:00","timeZone":"UTC"}}]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"value":[{"id":"first","subject":"First","start":{"dateTime":"2026-08-10T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-10T10:00:00","timeZone":"UTC"}}],"@odata.nextLink":%q}`, server.URL+"/next-events")
	}))
	defer server.Close()
	p := &Provider{client: server.Client(), baseURL: server.URL}

	got, err := p.GetEvents(context.Background(), "calendar", time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GetEvents() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("GetEvents() = %#v, want both pages", got)
	}
}

func TestV2CapabilitiesDoNotPromiseUnsupportedRecurrenceWrites(t *testing.T) {
	capabilities, err := (&Provider{}).Capabilities(context.Background(), "calendar")
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Fields.Recurrence || capabilities.SupportsScope(calendar.ScopeFollowing) {
		t.Fatalf("capabilities overstate recurrence support: %#v", capabilities)
	}
	if !capabilities.Fields.Conferencing || !capabilities.Fields.OptimisticLocking {
		t.Fatalf("capabilities omit supported fields: %#v", capabilities)
	}
}

func TestMicrosoftV2RejectsAttendeesWithoutExplicitNotifications(t *testing.T) {
	p := &Provider{}
	_, err := p.CreateEventV2(context.Background(), calendar.CreateEventRequestV2{Event: calendar.EventCreateV2{
		Start: calendar.EventTime{Date: "2026-08-10"}, End: calendar.EventTime{Date: "2026-08-11"},
		Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "person@example.com"}}},
	}})
	if err == nil {
		t.Fatal("CreateEventV2() allowed attendee write with notification_policy=none")
	}
}

func TestGraphPreconditionFailureMapsToConflict(t *testing.T) {
	err := graphAPIError(http.StatusPreconditionFailed, []byte(`{"error":"stale"}`))
	apiErr, ok := err.(*calendar.APIError)
	if !ok || apiErr.Code != calendar.ErrorConflict {
		t.Fatalf("error = %#v, want conflict", err)
	}
}
