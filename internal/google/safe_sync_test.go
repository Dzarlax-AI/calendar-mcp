package google

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	gcal "google.golang.org/api/calendar/v3"

	"calendar-mcp/internal/calendar"
)

func TestCacheSyncGoogleRequiresOwnershipAndSavedETag(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		for _, tc := range []struct {
			name, rule, etag string
			wantWrite        bool
		}{
			{"matching", "rule", `"v1"`, true},
			{"foreign-marker", "other-rule", `"v1"`, false},
			{"changed-etag", "rule", `"older"`, false},
			{"missing-etag", "rule", "", false},
		} {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				writes := 0
				p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, `{"id":"event","eventType":"default","etag":"\"v1\"","organizer":{"self":true},"attendees":[],"extendedProperties":{"private":{"calendar_sync_rule":"rule","calendar_source_event":"source"}},"start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`)
						return
					}
					writes++
					if r.Header.Get("If-Match") != `"v1"` || r.URL.Query().Get("sendUpdates") != "none" {
						t.Error("missing saved ETag or no-notification option")
					}
					if op == "delete" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					body, _ := io.ReadAll(r.Body)
					var payload map[string]json.RawMessage
					if err := json.Unmarshal(body, &payload); err != nil {
						t.Error(err)
					}
					if _, exists := payload["attendees"]; exists {
						t.Error("attendees sent")
					}
					_, _ = io.WriteString(w, `{"id":"event","etag":"\"v2\""}`)
				})
				defer closeServer()
				marker := &calendar.SyncMarker{RuleID: tc.rule, SourceEventID: "source"}
				var err error
				if op == "update" {
					_, err = p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true, ExpectedETag: tc.etag, ExpectedSyncMarker: marker})
				} else {
					_, err = p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true, ExpectedETag: tc.etag, ExpectedSyncMarker: marker})
				}
				if (writes == 1) != tc.wantWrite || (err == nil) != tc.wantWrite {
					t.Fatalf("writes=%d error=%v", writes, err)
				}
			})
		}
	}
}

func TestSafeSyncGoogleRejectsAttendeesAndUnknownBeforeMutation(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		for _, tc := range []struct{ name, json string }{
			{"attendees", `{"id":"event","eventType":"default","etag":"\"v1\"","organizer":{"self":true},"attendees":[{"email":"test@example.invalid"}]}`},
			{"omitted attendees", `{"id":"event","eventType":"default","etag":"\"v1\"","organizer":{"self":true},"attendees":[],"attendeesOmitted":true}`},
			{"unknown organizer", `{"id":"event","eventType":"default","etag":"\"v1\"","attendees":[]}`},
			{"not organizer", `{"id":"event","eventType":"default","etag":"\"v1\"","organizer":{"self":false},"attendees":[]}`},
			{"missing etag", `{"id":"event","eventType":"default","organizer":{"self":true},"attendees":[]}`},
		} {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				writes := 0
				p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, tc.json)
						return
					}
					writes++
					w.WriteHeader(http.StatusNoContent)
				})
				defer closeServer()
				var err error
				if op == "update" {
					_, err = p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
				} else {
					_, err = p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
				}
				if err == nil || writes != 0 {
					t.Fatalf("err=%v writes=%d", err, writes)
				}
			})
		}
	}
}

func TestSafeSyncGoogleUsesIfMatchAndNone(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		for _, conflict := range []bool{false, true} {
			t.Run(op+map[bool]string{false: "/success", true: "/race"}[conflict], func(t *testing.T) {
				writes := 0
				p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, `{"id":"event","eventType":"default","etag":"\"v1\"","organizer":{"self":true},"attendees":[],"start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`)
						return
					}
					writes++
					if op == "update" {
						body, _ := io.ReadAll(r.Body)
						var decoded map[string]json.RawMessage
						if err := json.Unmarshal(body, &decoded); err != nil {
							t.Errorf("invalid update body: %v", err)
						}
						if _, ok := decoded["attendees"]; ok {
							t.Error("update body contains attendees")
						}
						if _, ok := decoded["attendeesOmitted"]; ok {
							t.Error("update body contains attendeesOmitted")
						}
					}
					if r.Header.Get("If-Match") != `"v1"` || r.URL.Query().Get("sendUpdates") != "none" {
						t.Errorf("missing conditional notification protection")
					}
					if conflict {
						w.WriteHeader(http.StatusPreconditionFailed)
						_, _ = io.WriteString(w, `{"error":{"code":412,"message":"changed"}}`)
						return
					}
					if op == "delete" {
						w.WriteHeader(http.StatusNoContent)
					} else {
						_, _ = io.WriteString(w, `{"id":"event","eventType":"default","etag":"\"v2\"","start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`)
					}
				})
				defer closeServer()
				var err error
				if op == "update" {
					_, err = p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
				} else {
					_, err = p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
				}
				if writes != 1 || (err != nil) != conflict {
					t.Fatalf("err=%v writes=%d", err, writes)
				}
			})
		}
	}
}

func TestSafeSyncGoogleCreateForbidsAttendeesBeforeRequest(t *testing.T) {
	writes := 0
	p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) { writes++; w.WriteHeader(http.StatusInternalServerError) })
	defer closeServer()
	_, err := p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{CalendarID: "primary", Notifications: calendar.NotificationsNone, RequireNoAttendees: true, Event: calendar.EventCreateV2{Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "test@example.invalid"}}}}})
	if err == nil || writes != 0 {
		t.Fatalf("attendees allowed: %v %d", err, writes)
	}
	_, err = p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{CalendarID: "primary", Notifications: calendar.NotificationsAll, RequireNoAttendees: true})
	if err == nil || writes != 0 {
		t.Fatalf("notifications allowed: %v %d", err, writes)
	}
}

func TestGoogleGetAttendeesKnownOnlyOnFullOwnerResponse(t *testing.T) {
	for _, tc := range []struct {
		body  string
		known bool
	}{{`{"id":"event","eventType":"default","organizer":{"self":true},"attendees":[]}`, true}, {`{"id":"event","eventType":"default","organizer":{"self":true}}`, true}, {`{"id":"event","eventType":"default","organizer":{"self":true},"attendees":[],"attendeesOmitted":true}`, false}, {`{"id":"event","eventType":"default","attendees":[]}`, false}} {
		p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "/calendars/") && !strings.Contains(r.URL.Path, "/events/") {
				_, _ = io.WriteString(w, `{"timeZone":"UTC"}`)
				return
			}
			_, _ = io.WriteString(w, tc.body)
		})
		event, err := p.GetEventV2(t.Context(), calendar.EventRef{CalendarID: "primary", EventID: "event"})
		closeServer()
		if err != nil || event.AttendeesKnown != tc.known {
			t.Fatalf("known=%v err=%v", tc.known, err)
		}
	}
}

func TestGoogleSourceAttendeesCompletenessIsStrict(t *testing.T) {
	visible := true
	tests := []struct {
		name  string
		event gcal.Event
		want  bool
	}{
		{"explicit empty organizer", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{}}, true},
		{"guest visibility without self is unknown", gcal.Event{GuestsCanSeeOtherGuests: &visible, Attendees: []*gcal.EventAttendee{}}, false},
		{"missing array", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}}, false},
		{"omitted attendees", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, AttendeesOmitted: true, Attendees: []*gcal.EventAttendee{}}, false},
		{"unknown visibility", gcal.Event{Attendees: []*gcal.EventAttendee{}}, false},
		{"explicit participants fully visible", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{{Email: "person@example.invalid"}}}, true},
		{"guest with self and allowed visibility", gcal.Event{GuestsCanSeeOtherGuests: &visible, Attendees: []*gcal.EventAttendee{{Email: "self@example.invalid", Self: true}, {Email: "other@example.invalid"}}}, true},
		{"guest without self is unknown", gcal.Event{GuestsCanSeeOtherGuests: &visible, Attendees: []*gcal.EventAttendee{{Email: "other@example.invalid"}}}, false},
		{"pending group expansion", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{{Email: "group@example.invalid", AsyncOperation: "inProgress"}}}, false},
		{"unknown async status", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{{Email: "group@example.invalid", AsyncOperation: "future-status"}}}, false},
		{"additional unnamed guests", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{{Email: "person@example.invalid", AdditionalGuests: 1}}}, false},
		{"anonymous attendee", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{{DisplayName: "Anonymous"}}}, false},
		{"nil attendee", gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{nil}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fromGoogleEventV2(&tc.event, "calendar", "")
			if got.SourceAttendeesComplete != tc.want {
				t.Fatalf("complete=%v want %v", got.SourceAttendeesComplete, tc.want)
			}
		})
	}
}

func TestSafeSyncGoogleCreateWritesNoAttendees(t *testing.T) {
	writes := 0
	p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writes++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("invalid body: %v", err)
		}
		for _, field := range []string{"attendees", "organizer", "conferenceData", "recurrence"} {
			if _, ok := payload[field]; ok {
				t.Errorf("unsafe create field: %s", field)
			}
		}
		if r.Method != http.MethodPost || r.URL.Query().Get("sendUpdates") != "none" || !strings.Contains(string(body), "Known participants") {
			t.Errorf("unsafe create request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"created","etag":"\"v1\"","start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`)
	})
	defer closeServer()
	_, err := p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{CalendarID: "primary", Notifications: calendar.NotificationsNone, RequireNoAttendees: true, Event: calendar.EventCreateV2{Title: "synthetic", Description: "<strong>Known participants</strong><br>person@example.invalid", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}})
	if err != nil || writes != 1 {
		t.Fatalf("err=%v writes=%d", err, writes)
	}
}

func TestSourceAttendeeCompletenessIsNotTrustableAfterJSONRoundTrip(t *testing.T) {
	source := fromGoogleEventV2(&gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, Attendees: []*gcal.EventAttendee{}}, "calendar", "")
	if !source.SourceAttendeesComplete {
		t.Fatal("precondition")
	}
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var restored calendar.EventV2
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.SourceAttendeesComplete {
		t.Fatal("unbound completeness survived JSON serialization")
	}
}

func TestGoogleSourceCompletenessAfterProviderJSONDecoding(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		want       bool
	}{
		{"no async field", `{"organizer":{"self":true},"attendees":[{"email":"person@example.invalid"}]}`, true},
		{"in progress", `{"organizer":{"self":true},"attendees":[{"email":"group@example.invalid","asyncOperation":"inProgress"}]}`, false},
		{"additional guests", `{"organizer":{"self":true},"attendees":[{"email":"person@example.invalid","additionalGuests":1}]}`, false},
		{"omitted", `{"organizer":{"self":true},"attendeesOmitted":true,"attendees":[{"email":"person@example.invalid"}]}`, false},
		{"null participant", `{"organizer":{"self":true},"attendees":[null]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw gcal.Event
			if err := json.Unmarshal([]byte(tc.json), &raw); err != nil {
				t.Fatal(err)
			}
			got := fromGoogleEventV2(&raw, "calendar", "")
			if got.SourceAttendeesComplete != tc.want {
				t.Fatalf("complete=%v want=%v", got.SourceAttendeesComplete, tc.want)
			}
		})
	}
}

func TestSafeSyncGoogleRejectsEveryAttendeePatchBeforeWrite(t *testing.T) {
	for _, patch := range []calendar.PatchField[[]calendar.AttendeeV2]{
		{Present: true, Value: []calendar.AttendeeV2{}},
		{Present: true, Null: true},
		{Present: true, Value: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "person@example.invalid"}}}},
	} {
		writes := 0
		p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"id":"event","eventType":"default","etag":"\"v1\"","organizer":{"self":true},"attendees":[],"start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`)
				return
			}
			writes++
		})
		_, err := p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true, Patch: calendar.EventPatchV2{Attendees: patch}})
		closeServer()
		if err == nil || writes != 0 {
			t.Fatalf("attendee patch accepted: err=%v writes=%d", err, writes)
		}
	}
}

func TestSafeSyncGoogleBlocksMasterEvenIfOnlyExceptionHasAttendees(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		t.Run(op, func(t *testing.T) {
			writes := 0
			p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if strings.Contains(r.URL.Path, "/instances") {
						_, _ = io.WriteString(w, `{"items":[{"id":"exception","attendees":[{"email":"person@example.invalid"}]}]}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"master","eventType":"default","etag":"\"v1\"","organizer":{"self":true},"attendees":[],"recurrence":["RRULE:FREQ=DAILY"],"start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`)
					return
				}
				writes++
			})
			defer closeServer()
			var err error
			if op == "update" {
				_, err = p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "master"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
			} else {
				_, err = p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "master"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
			}
			if err == nil || writes != 0 {
				t.Fatalf("master mutation allowed: err=%v writes=%d", err, writes)
			}
		})
	}
}

func TestSafeSyncGoogleBlocksRecurringAndConferencingCreate(t *testing.T) {
	for _, event := range []calendar.EventCreateV2{
		{Recurrence: []string{"RRULE:FREQ=DAILY"}},
		{Conference: &calendar.ConferenceData{Solution: "video"}},
		{Google: &calendar.GoogleEventExtension{EventType: "outOfOffice"}},
	} {
		writes := 0
		p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) { writes++ })
		_, err := p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{CalendarID: "primary", Notifications: calendar.NotificationsNone, RequireNoAttendees: true, Event: event})
		closeServer()
		if err == nil || writes != 0 {
			t.Fatalf("unsafe create allowed: err=%v writes=%d", err, writes)
		}
	}
}

func TestSafeSyncGoogleRejectsUnsafePatchWithoutProviderRequests(t *testing.T) {
	patches := []calendar.EventPatchV2{
		{Attendees: calendar.PatchField[[]calendar.AttendeeV2]{Present: true, Value: []calendar.AttendeeV2{}}},
		{Recurrence: calendar.PatchField[[]string]{Present: true, Value: []string{"RRULE:FREQ=DAILY"}}},
		{Conference: calendar.PatchField[calendar.ConferenceData]{Present: true, Value: calendar.ConferenceData{Solution: "video"}}},
		{Google: calendar.PatchField[calendar.GoogleEventExtension]{Present: true, Value: calendar.GoogleEventExtension{EventType: "outOfOffice"}}},
		{GuestPermissions: calendar.PatchField[calendar.GuestPermissions]{Present: true}},
	}
	for _, patch := range patches {
		requests := 0
		p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) { requests++ })
		_, err := p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true, Patch: patch})
		closeServer()
		if err == nil || requests != 0 {
			t.Fatalf("unsafe patch made provider request: err=%v requests=%d", err, requests)
		}
	}
}

func TestSafeSyncGoogleRejectsSpecialAndConferenceTargets(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		for _, tc := range []struct{ name, extra string }{
			{"unknown type", ""},
			{"out of office", `,"eventType":"outOfOffice"`},
			{"focus time", `,"eventType":"focusTime"`},
			{"conference", `,"eventType":"default","conferenceData":{"conferenceId":"synthetic"}`},
			{"hangout link", `,"eventType":"default","hangoutLink":"https://example.invalid/meet"`},
		} {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				writes := 0
				p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, `{"id":"event","etag":"\"v1\"","organizer":{"self":true},"attendees":[]`+tc.extra+`}`)
						return
					}
					writes++
				})
				defer closeServer()
				var err error
				if op == "update" {
					_, err = p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
				} else {
					_, err = p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
				}
				if err == nil || writes != 0 {
					t.Fatalf("unsafe target allowed: err=%v writes=%d", err, writes)
				}
			})
		}
	}
}
