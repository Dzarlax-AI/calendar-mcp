package google

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"calendar-mcp/internal/calendar"
	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

func TestMeetingMirrorFakeGoogle(t *testing.T) {
	oldURL := "https://meet.google.com/abc-defg-hij?authuser=1"
	oldDescription, _ := calendar.MergeMirrorMeeting("manual and participants", oldURL)
	for _, tc := range []struct {
		name              string
		update            calendar.MirrorMeetingUpdate
		conflict, foreign bool
		wantURL           string
		body              string
		wantBody          string
	}{
		{name: "replace", update: calendar.MirrorMeetingUpdate{Known: true, URL: "https://us02web.zoom.us/j/123?pwd=a%2Bb"}, wantURL: "https://us02web.zoom.us/j/123?pwd=a%2Bb"},
		{name: "replace meet", update: calendar.MirrorMeetingUpdate{Known: true, URL: "https://meet.google.com/new-meet-abc?authuser=2"}, wantURL: "https://meet.google.com/new-meet-abc?authuser=2"},
		{name: "replace teams", update: calendar.MirrorMeetingUpdate{Known: true, URL: "https://teams.microsoft.com/l/meetup-join/id?context=a%2Fb"}, wantURL: "https://teams.microsoft.com/l/meetup-join/id?context=a%2Fb"},
		{name: "remove", update: calendar.MirrorMeetingUpdate{Known: true}},
		{name: "unknown", update: calendar.MirrorMeetingUpdate{}, wantURL: oldURL},
		{name: "source body with known meeting", update: calendar.MirrorMeetingUpdate{Known: true, URL: oldURL}, body: "new agenda and participants", wantBody: "new agenda and participants", wantURL: oldURL},
		{name: "source body with unknown meeting", update: calendar.MirrorMeetingUpdate{}, body: "new agenda and participants", wantBody: "new agenda and participants", wantURL: oldURL},
		{name: "etag", update: calendar.MirrorMeetingUpdate{Known: true}, conflict: true},
		{name: "owner", update: calendar.MirrorMeetingUpdate{Known: true}, foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			client := &http.Client{Transport: dispatchRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}
				if r.Method == http.MethodGet {
					owner := "rule"
					if tc.foreign {
						owner = "other"
					}
					data, _ := json.Marshal(map[string]any{"id": "event", "eventType": "default", "etag": `"v1"`, "organizer": map[string]any{"self": true}, "attendees": []any{}, "description": oldDescription, "extendedProperties": map[string]any{"private": map[string]string{"calendar_sync_rule": owner, "calendar_source_event": "source"}}, "start": map[string]string{"date": "2026-11-05"}, "end": map[string]string{"date": "2026-11-06"}})
					response.Body = io.NopCloser(strings.NewReader(string(data)))
					return response, nil
				}
				writes++
				if r.Header.Get("If-Match") != `"v1"` || r.URL.Query().Get("sendUpdates") != "none" {
					t.Error("missing write safety")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if _, ok := body["attendees"]; ok {
					t.Error("attendees sent")
				}
				if _, ok := body["conferenceData"]; ok {
					t.Error("conference sent")
				}
				var description string
				_ = json.Unmarshal(body["description"], &description)
				wantBody := tc.wantBody
				if wantBody == "" {
					wantBody = "manual and participants"
				}
				if !strings.Contains(description, wantBody) || (tc.wantURL != "" && !strings.Contains(description, tc.wantURL)) || (tc.wantURL == "" && strings.Contains(description, "calendar-mcp:meeting:start")) {
					t.Error("description merge failed")
				}
				if tc.conflict {
					response.StatusCode = 412
					response.Body = io.NopCloser(strings.NewReader(`{"error":{"code":412}}`))
					return response, nil
				}
				response.Body = io.NopCloser(strings.NewReader(`{"id":"event","etag":"v2"}`))
				return response, nil
			})}
			svc, err := gcal.NewService(t.Context(), option.WithHTTPClient(client))
			if err != nil {
				t.Fatal(err)
			}
			patch := calendar.EventPatchV2{MirrorMeeting: &tc.update}
			if tc.body != "" {
				patch.Description = calendar.PatchField[string]{Present: true, Value: tc.body}
			}
			_, err = (&Provider{svc: svc}).UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true, ExpectedETag: `"v1"`, ExpectedSyncMarker: &calendar.SyncMarker{RuleID: "rule", SourceEventID: "source"}, Patch: patch})
			if tc.foreign {
				if err == nil || writes != 0 {
					t.Fatal("foreign owner accepted")
				}
			} else if tc.conflict {
				if err == nil || writes != 1 {
					t.Fatal("conflict not returned")
				}
			} else if err != nil || writes != 1 {
				t.Fatalf("write failed: %v", err)
			}
		})
	}
}

func TestGoogleMeetingSourceEvidence(t *testing.T) {
	raw := "https://meet.google.com/abc-defg-hij?authuser=1"
	event := &gcal.Event{Organizer: &gcal.EventOrganizer{Self: true}, ConferenceData: &gcal.ConferenceData{EntryPoints: []*gcal.EntryPoint{{EntryPointType: "video", Uri: raw}}}}
	got := fromGoogleEventV2(event, "calendar", "UTC")
	if !got.SourceConferenceComplete || calendar.MirrorMeetingURL(got.Conference) != raw {
		t.Fatal("Google conference projection lost")
	}
	event.ConferenceData = nil
	event.HangoutLink = raw
	got = fromGoogleEventV2(event, "calendar", "UTC")
	if !got.SourceConferenceComplete || calendar.MirrorMeetingURL(got.Conference) != raw {
		t.Fatal("legacy Google video link was lost")
	}
	event.HangoutLink = "https://example.invalid/unrecognized"
	if fromGoogleEventV2(event, "calendar", "UTC").SourceConferenceComplete {
		t.Fatal("unrecognized Google video link treated as confirmed absence")
	}
	event.HangoutLink = ""
	got = fromGoogleEventV2(event, "calendar", "UTC")
	if !got.SourceConferenceComplete {
		t.Fatal("owned absence not confirmed")
	}
	event.Organizer = nil
	got = fromGoogleEventV2(event, "calendar", "UTC")
	if got.SourceConferenceComplete {
		t.Fatal("unverified absence marked complete")
	}
}
