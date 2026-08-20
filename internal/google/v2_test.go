package google

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	gcal "google.golang.org/api/calendar/v3"

	"calendar-mcp/internal/calendar"
)

func TestCreateEventV2MapsRecurringEventAndSafetyOptions(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.URL.Query().Get("sendUpdates"); got != "externalOnly" {
			t.Errorf("sendUpdates = %q, want externalOnly", got)
		}
		if got := r.URL.Query().Get("supportsAttachments"); got != "true" {
			t.Errorf("supportsAttachments = %q, want true", got)
		}
		if got := r.URL.Query().Get("conferenceDataVersion"); got != "1" {
			t.Errorf("conferenceDataVersion = %q, want 1", got)
		}
		body, _ := io.ReadAll(r.Body)
		for _, want := range []string{`"recurrence":["RRULE:FREQ=WEEKLY;BYDAY=MO,WE"]`, `"timeZone":"Europe/Belgrade"`, `"fileUrl":"https://example.test/file"`, `"requestId":"unique-request"`} {
			if !bytes.Contains(body, []byte(want)) {
				t.Errorf("request body %s does not contain %s", body, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"series","etag":"etag-1","summary":"Standup","recurrence":["RRULE:FREQ=WEEKLY;BYDAY=MO,WE"],"start":{"dateTime":"2026-08-10T09:00:00+02:00","timeZone":"Europe/Belgrade"},"end":{"dateTime":"2026-08-10T09:30:00+02:00","timeZone":"Europe/Belgrade"}}`)
	})
	defer closeServer()

	result, err := provider.CreateEventV2(context.Background(), calendar.CreateEventRequestV2{
		CalendarID:    "primary",
		Notifications: calendar.NotificationsExternalOnly,
		Event: calendar.EventCreateV2{
			Title:       "Standup",
			Start:       timedEventTime("2026-08-10T09:00:00+02:00"),
			End:         timedEventTime("2026-08-10T09:30:00+02:00"),
			Recurrence:  []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,WE"},
			Attachments: []calendar.Attachment{{FileURL: "https://example.test/file"}},
			Conference:  &calendar.ConferenceData{RequestID: "unique-request"},
		},
	})
	if err != nil {
		t.Fatalf("CreateEventV2() error = %v", err)
	}
	if result.ID != "series" || result.ETag != "etag-1" || len(result.Recurrence) != 1 {
		t.Fatalf("CreateEventV2() = %#v", result)
	}
}

func TestFindEventBySyncMarkerV2UsesBothPrivateProperties(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		properties := r.URL.Query()["privateExtendedProperty"]
		joined := strings.Join(properties, "|")
		for _, want := range []string{"calendar_sync_rule=rule", "calendar_source_event=source"} {
			if !strings.Contains(joined, want) {
				t.Errorf("privateExtendedProperty = %v, missing %q", properties, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"recovered","summary":"Recovered","start":{"dateTime":"2026-08-10T09:00:00Z"},"end":{"dateTime":"2026-08-10T10:00:00Z"}}]}`)
	})
	defer closeServer()

	event, err := provider.FindEventBySyncMarkerV2(context.Background(), "primary", "rule", "source")
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.ID != "recovered" {
		t.Fatalf("event = %#v", event)
	}
}

func TestUpdateEventV2UsesETagAndExplicitlyClearsFields(t *testing.T) {
	requestCount := 0
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"id":"event","etag":"old","summary":"Existing","description":"Remove","attendees":[{"email":"person@example.com"}],"recurrence":["RRULE:FREQ=DAILY"],"start":{"dateTime":"2026-08-10T09:00:00+02:00","timeZone":"Europe/Belgrade"},"end":{"dateTime":"2026-08-10T10:00:00+02:00","timeZone":"Europe/Belgrade"}}`)
			return
		}
		if got := r.Header.Get("If-Match"); got != `"expected"` {
			t.Errorf("If-Match = %q, want quoted expected ETag", got)
		}
		if got := r.URL.Query().Get("sendUpdates"); got != "none" {
			t.Errorf("sendUpdates = %q, want none", got)
		}
		body, _ := io.ReadAll(r.Body)
		for _, want := range []string{`"description":null`, `"attendees":[]`, `"recurrence":[]`} {
			if !bytes.Contains(body, []byte(want)) {
				t.Errorf("request body %s does not contain %s", body, want)
			}
		}
		_, _ = io.WriteString(w, `{"id":"event","etag":"new","summary":"Existing","start":{"dateTime":"2026-08-10T09:00:00+02:00","timeZone":"Europe/Belgrade"},"end":{"dateTime":"2026-08-10T10:00:00+02:00","timeZone":"Europe/Belgrade"}}`)
	})
	defer closeServer()

	result, err := provider.UpdateEventV2(context.Background(), calendar.UpdateEventRequestV2{
		Ref:           calendar.EventRef{CalendarID: "primary", EventID: "event"},
		Scope:         calendar.ScopeSeries,
		ExpectedETag:  `"expected"`,
		Notifications: calendar.NotificationsNone,
		Patch: calendar.EventPatchV2{
			Description: calendar.PatchField[string]{Present: true, Null: true},
			Attendees:   calendar.PatchField[[]calendar.AttendeeV2]{Present: true, Value: []calendar.AttendeeV2{}},
			Recurrence:  calendar.PatchField[[]string]{Present: true, Value: []string{}},
		},
	})
	if err != nil {
		t.Fatalf("UpdateEventV2() error = %v", err)
	}
	if requestCount != 2 || result.Status != "completed" || result.Event == nil || result.Event.ETag != "new" {
		t.Fatalf("UpdateEventV2() = %#v, requests = %d", result, requestCount)
	}
}

func TestUpdateEventV2UsesFetchedETagWhenExpectedETagIsOmitted(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"id":"event","etag":"fetched-etag","start":{"dateTime":"2026-08-10T09:00:00+02:00","timeZone":"Europe/Belgrade"},"end":{"dateTime":"2026-08-10T10:00:00+02:00","timeZone":"Europe/Belgrade"}}`)
			return
		}
		if got := r.Header.Get("If-Match"); got != "fetched-etag" {
			t.Errorf("If-Match = %q, want fetched-etag", got)
		}
		_, _ = io.WriteString(w, `{"id":"event","etag":"updated","start":{"dateTime":"2026-08-10T09:00:00+02:00","timeZone":"Europe/Belgrade"},"end":{"dateTime":"2026-08-10T10:00:00+02:00","timeZone":"Europe/Belgrade"}}`)
	})
	defer closeServer()
	_, err := provider.UpdateEventV2(context.Background(), calendar.UpdateEventRequestV2{
		Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries,
		Patch: calendar.EventPatchV2{Title: calendar.PatchField[string]{Present: true, Value: "Changed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFromGmailPatchRejectsImmutableFields(t *testing.T) {
	event := &gcal.Event{EventType: "fromGmail"}
	err := validateGooglePatchForEventType(event, calendar.EventPatchV2{Title: calendar.PatchField[string]{Present: true, Value: "Changed"}})
	if err == nil {
		t.Fatal("fromGmail title update was accepted")
	}
}

func TestDeleteEventV2UsesNotificationPolicyAndETag(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if got := r.URL.Query().Get("sendUpdates"); got != "all" {
			t.Errorf("sendUpdates = %q, want all", got)
		}
		if got := r.Header.Get("If-Match"); got != "etag" {
			t.Errorf("If-Match = %q, want etag", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer closeServer()

	result, err := provider.DeleteEventV2(context.Background(), calendar.DeleteEventRequestV2{
		Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSingle,
		ExpectedETag: "etag", Notifications: calendar.NotificationsAll,
	})
	if err != nil {
		t.Fatalf("DeleteEventV2() error = %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed", result.Status)
	}
}

func TestCreateEventV2RejectsInvalidSpecialEventsBeforeNetwork(t *testing.T) {
	tests := []struct {
		name  string
		event calendar.EventCreateV2
		want  string
	}{
		{
			name: "from Gmail",
			event: calendar.EventCreateV2{Start: timedEventTime("2026-08-10T09:00:00+02:00"), End: timedEventTime("2026-08-10T10:00:00+02:00"),
				Google: &calendar.GoogleEventExtension{EventType: "fromGmail"}},
			want: "cannot be created",
		},
		{
			name: "birthday without yearly recurrence",
			event: calendar.EventCreateV2{Start: calendar.EventTime{Date: "2026-08-10"}, End: calendar.EventTime{Date: "2026-08-11"},
				Google: &calendar.GoogleEventExtension{EventType: "birthday", Birthday: map[string]any{"type": "birthday"}}},
			want: "annual RRULE",
		},
		{
			name: "working location across multiple days",
			event: calendar.EventCreateV2{Start: calendar.EventTime{Date: "2026-08-10"}, End: calendar.EventTime{Date: "2026-08-12"},
				Google: &calendar.GoogleEventExtension{EventType: "workingLocation", WorkingLocation: map[string]any{"type": "homeOffice"}}},
			want: "exactly one day",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, closeServer := testProvider(t, func(http.ResponseWriter, *http.Request) {
				t.Fatal("network request made for invalid event")
			})
			defer closeServer()
			_, err := provider.CreateEventV2(context.Background(), calendar.CreateEventRequestV2{CalendarID: "primary", Event: tt.event})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CreateEventV2() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFromGoogleEventV2PreservesRecurrenceAndTimezone(t *testing.T) {
	data := []byte(`{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2026-08-10T09:00:00+02:00","timeZone":"Europe/Belgrade"},"recurrence":["RRULE:FREQ=WEEKLY"],"start":{"dateTime":"2026-08-10T09:00:00+02:00","timeZone":"Europe/Belgrade"},"end":{"dateTime":"2026-08-10T10:00:00+02:00","timeZone":"Europe/Belgrade"}}`)
	var event struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	defer closeServer()
	result, err := provider.GetEventV2(context.Background(), calendar.EventRef{CalendarID: "primary", EventID: event.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.RecurringEventID != "series" || result.OriginalStart == nil || result.Start.TimeZone != "Europe/Belgrade" || result.Start.DateTime != "2026-08-10T09:00:00+02:00" {
		t.Fatalf("mapped event = %#v", result)
	}
}

func timedEventTime(value string) calendar.EventTime {
	return calendar.EventTime{DateTime: value, TimeZone: "Europe/Belgrade"}
}

func TestGoogleInstanceDiffersClassifiesOnlyOverrides(t *testing.T) {
	start := calendar.EventTime{DateTime: "2026-08-21T09:00:00Z", TimeZone: "UTC"}
	end := calendar.EventTime{DateTime: "2026-08-21T10:00:00Z", TimeZone: "UTC"}
	master := calendar.EventV2{Title: "Daily", Description: "Status", Start: start, End: end}
	original := start
	ordinary := calendar.EventV2{Title: "Daily", Description: "Status", Start: start, End: end, OriginalStart: &original}
	if googleInstanceDiffers(master, ordinary) {
		t.Fatal("ordinary instance classified as exception")
	}
	moved := ordinary
	moved.Start = calendar.EventTime{DateTime: "2026-08-21T11:00:00Z", TimeZone: "UTC"}
	moved.End = calendar.EventTime{DateTime: "2026-08-21T12:00:00Z", TimeZone: "UTC"}
	if !googleInstanceDiffers(master, moved) {
		t.Fatal("moved instance was not classified as exception")
	}
	renamed := ordinary
	renamed.Title = "One-off title"
	if !googleInstanceDiffers(master, renamed) {
		t.Fatal("renamed instance was not classified as exception")
	}
}
