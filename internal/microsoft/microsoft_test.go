package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestV2CapabilitiesAdvertiseReadableRecurrenceWithoutFollowingWrites(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"canEdit":true}`)
	}))
	defer server.Close()
	capabilities, err := (&Provider{client: server.Client(), baseURL: server.URL}).Capabilities(context.Background(), "calendar")
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Fields.Recurrence || capabilities.SupportsScope(calendar.ScopeFollowing) {
		t.Fatalf("capabilities overstate recurrence support: %#v", capabilities)
	}
	if !capabilities.Fields.Conferencing || !capabilities.Fields.OptimisticLocking {
		t.Fatalf("capabilities omit supported fields: %#v", capabilities)
	}
}

func TestGraphCreateCarriesSyncMarker(t *testing.T) {
	body, err := toGraphCreateV2(calendar.EventCreateV2{Start: calendar.EventTime{DateTime: "2026-08-20T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T10:00:00Z", TimeZone: "UTC"}, SyncMarker: &calendar.SyncMarker{RuleID: "rule", SourceEventID: "source"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.SingleValueExtendedProperties) != 1 || body.SingleValueExtendedProperties[0].Value != calendar.SyncMarkerValue("rule", "source") {
		t.Fatalf("extended properties = %#v", body.SingleValueExtendedProperties)
	}
}

func TestGraphEventNormalizesInstanceKinds(t *testing.T) {
	for _, test := range []struct {
		name, eventType, want string
		cancelled             bool
	}{
		{name: "standalone", eventType: "singleInstance", want: ""},
		{name: "cancelled occurrence", eventType: "occurrence", cancelled: true, want: "cancelled"},
		{name: "cancelled exception", eventType: "exception", cancelled: true, want: "cancelled"},
		{name: "cancelled master", eventType: "seriesMaster", cancelled: true, want: "seriesMaster"},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, err := (&graphEvent{Type: test.eventType, IsCancelled: test.cancelled}).toEventV2("calendar")
			if err != nil {
				t.Fatal(err)
			}
			if event.InstanceKind != test.want {
				t.Fatalf("instance kind = %q, want %q", event.InstanceKind, test.want)
			}
		})
	}
}

func TestGraphEventPreservesDescriptionContentType(t *testing.T) {
	event, err := (&graphEvent{Body: struct {
		Content     string `json:"content"`
		ContentType string `json:"contentType"`
	}{Content: "<p>Agenda</p>", ContentType: "html"}}).toEventV2("calendar")
	if err != nil {
		t.Fatal(err)
	}
	if event.Description != "<p>Agenda</p>" || event.DescriptionFormat != "html" {
		t.Fatalf("description metadata = %#v", event)
	}
}

func TestMicrosoftUpdatePreservesExistingHTMLDescriptionFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"id":"event","body":{"content":"<p>Agenda</p>","contentType":"html"},"start":{"dateTime":"2026-08-10T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-10T10:00:00","timeZone":"UTC"}}`)
		case http.MethodPatch:
			var patch struct {
				Body graphBody `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Fatal(err)
			}
			if patch.Body.ContentType != "html" || patch.Body.Content != "<p>Updated agenda</p>" {
				t.Fatalf("body patch = %#v", patch.Body)
			}
			_, _ = io.WriteString(w, `{"id":"event","body":{"content":"<p>Updated agenda</p>","contentType":"html"},"start":{"dateTime":"2026-08-10T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-10T10:00:00","timeZone":"UTC"}}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	_, err := (&Provider{client: server.Client(), baseURL: server.URL}).UpdateEventV2(context.Background(), calendar.UpdateEventRequestV2{
		Ref: calendar.EventRef{CalendarID: "calendar", EventID: "event"}, Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone,
		Patch: calendar.EventPatchV2{Description: calendar.PatchField[string]{Present: true, Value: "<p>Updated agenda</p>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFindEventBySyncMarkerV2UsesFilteredExtendedProperty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("$filter"), calendar.SyncMarkerValue("rule", "source")) {
			t.Errorf("filter = %q", r.URL.Query().Get("$filter"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"value":[{"id":"recovered","start":{"dateTime":"2026-08-20T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-20T10:00:00","timeZone":"UTC"}}]}`)
	}))
	defer server.Close()
	event, err := (&Provider{client: server.Client(), baseURL: server.URL}).FindEventBySyncMarkerV2(context.Background(), "calendar", "rule", "source")
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.ID != "recovered" {
		t.Fatalf("event = %#v", event)
	}
}

func TestListEventsV2BothReturnsMasterExceptionAndBoundedCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/calendars/calendar/calendarView":
			_, _ = io.WriteString(w, `{"value":[{"id":"exception","subject":"Moved","type":"exception","seriesMasterId":"master","originalStart":"2026-08-21T09:00:00Z","start":{"dateTime":"2026-08-21T11:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-21T12:00:00","timeZone":"UTC"}}]}`)
		case "/me/calendars/calendar/events/master":
			_, _ = io.WriteString(w, `{"id":"master","subject":"Daily","type":"seriesMaster","start":{"dateTime":"2026-08-20T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-20T10:00:00","timeZone":"UTC"},"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"2026-08-20","recurrenceTimeZone":"UTC"}},"cancelledOccurrences":["OID.master.2026-08-22","OID.master.2026-09-30"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	page, err := (&Provider{client: server.Client(), baseURL: server.URL}).ListEventsV2(context.Background(), calendar.ListEventsRequestV2{
		CalendarID: "calendar",
		Start:      time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		End:        time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC),
		View:       calendar.RecurrenceBoth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("items = %#v", page.Items)
	}
	if page.Items[0].InstanceKind != "seriesMaster" || len(page.Items[0].Recurrence) != 1 {
		t.Fatalf("master = %#v", page.Items[0])
	}
	if page.Items[1].InstanceKind != "cancelled" || page.Items[1].OriginalStart == nil || page.Items[1].OriginalStart.DateTime != "2026-08-22T09:00:00Z" {
		t.Fatalf("cancellation = %#v", page.Items[1])
	}
	if page.Items[2].InstanceKind != "exception" || page.Items[2].OriginalStart == nil {
		t.Fatalf("exception = %#v", page.Items[2])
	}
}

func TestListEventsV2BothConsumesPaginationBeforeAddingMasters(t *testing.T) {
	masterCalls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/calendars/calendar/calendarView":
			_, _ = fmt.Fprintf(w, `{"value":[{"id":"one","type":"occurrence","seriesMasterId":"master","start":{"dateTime":"2026-08-20T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-20T10:00:00","timeZone":"UTC"}}],"@odata.nextLink":%q}`, server.URL+"/next-view")
		case "/next-view":
			_, _ = io.WriteString(w, `{"value":[{"id":"two","type":"occurrence","seriesMasterId":"master","start":{"dateTime":"2026-08-21T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-21T10:00:00","timeZone":"UTC"}}]}`)
		case "/me/calendars/calendar/events/master":
			masterCalls++
			_, _ = io.WriteString(w, `{"id":"master","type":"seriesMaster","start":{"dateTime":"2026-08-20T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-20T10:00:00","timeZone":"UTC"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	page, err := (&Provider{client: server.Client(), baseURL: server.URL}).ListEventsV2(context.Background(), calendar.ListEventsRequestV2{CalendarID: "calendar", Start: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC), View: calendar.RecurrenceBoth})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || page.NextPageToken != "" || masterCalls != 1 {
		t.Fatalf("page=%#v master calls=%d", page, masterCalls)
	}
}

func TestMicrosoftUpdateRejectsNoneWhenExistingEventHasAttendees(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected mutation request: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"event","attendees":[{"emailAddress":{"address":"person@example.com"}}],"start":{"dateTime":"2026-08-10T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-08-10T10:00:00","timeZone":"UTC"}}`)
	}))
	defer server.Close()
	p := &Provider{client: server.Client(), baseURL: server.URL}
	_, err := p.UpdateEventV2(context.Background(), calendar.UpdateEventRequestV2{
		Ref: calendar.EventRef{CalendarID: "calendar", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone,
		Patch: calendar.EventPatchV2{Title: calendar.PatchField[string]{Present: true, Value: "Changed"}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot suppress update messages") {
		t.Fatalf("UpdateEventV2() error = %v", err)
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

func TestValidateGraphPageURLRejectsDifferentOrigin(t *testing.T) {
	p := &Provider{baseURL: "https://graph.microsoft.com/v1.0"}
	if err := p.validateGraphPageURL("https://graph.microsoft.com/v1.0/me/events?$skiptoken=safe"); err != nil {
		t.Fatalf("valid Graph URL rejected: %v", err)
	}
	if err := p.validateGraphPageURL("https://example.test/collect-token"); err == nil {
		t.Fatal("external Graph page URL was accepted")
	}
}

func TestGraphPreconditionFailureMapsToConflict(t *testing.T) {
	err := graphAPIError(http.StatusPreconditionFailed, []byte(`{"error":"stale"}`))
	apiErr, ok := err.(*calendar.APIError)
	if !ok || apiErr.Code != calendar.ErrorConflict {
		t.Fatalf("error = %#v, want conflict", err)
	}
}
