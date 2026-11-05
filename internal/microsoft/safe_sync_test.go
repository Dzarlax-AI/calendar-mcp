package microsoft

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"calendar-mcp/internal/calendar"
)

const safeGraphTimes = `,"start":{"dateTime":"2026-11-05T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-05T10:00:00","timeZone":"UTC"}`

func TestSafeSyncMicrosoftBlocksUpdateDeleteUntilCASVerified(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		t.Run(op, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
			defer server.Close()
			p := &Provider{client: server.Client(), baseURL: server.URL}
			if p.SupportsNoAttendeeMutations() {
				t.Fatal("unverified capability advertised")
			}
			var err error
			if op == "update" {
				_, err = p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "calendar", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
			} else {
				_, err = p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "calendar", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
			}
			if err == nil || requests != 0 {
				t.Fatalf("unsafe graph mutation reached provider: err=%v requests=%d", err, requests)
			}
		})
	}
}

func TestSafeSyncMicrosoftCreateBlocksBeforeProviderRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	p := &Provider{client: server.Client(), baseURL: server.URL}
	_, err := p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{CalendarID: "calendar", Notifications: calendar.NotificationsNone, RequireNoAttendees: true, Event: calendar.EventCreateV2{Description: "<strong>Known participants</strong>", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}})
	if err == nil || requests != 0 {
		t.Fatalf("unsafe Graph create reached provider: err=%v requests=%d", err, requests)
	}
}

func TestMicrosoftSourceAttendeesCompletenessIsStrict(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		want       bool
	}{
		{"organizer and explicit empty", `"isOrganizer":true,"attendees":[]`, true},
		{"organizer with address", `"isOrganizer":true,"attendees":[{"emailAddress":{"address":"person@example.invalid"}}]`, true},
		{"missing address", `"isOrganizer":true,"attendees":[{"emailAddress":{}}]`, false},
		{"missing array", `"isOrganizer":true`, false},
		{"unknown organizer", `"attendees":[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event graphEvent
			if err := json.Unmarshal([]byte(`{"id":"event",`+tc.json+safeGraphTimes+`}`), &event); err != nil {
				t.Fatal(err)
			}
			got, err := event.toEventV2("calendar")
			if err != nil || got.SourceAttendeesComplete != tc.want {
				t.Fatalf("complete=%v want=%v err=%v", got.SourceAttendeesComplete, tc.want, err)
			}
		})
	}
}
