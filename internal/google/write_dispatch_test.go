package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
	"net/http"
	"testing"

	"calendar-mcp/internal/calendar"
)

type dispatchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dispatchRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type recordingGuard struct{ released int }

func (g *recordingGuard) BeginWriteDispatch(context.Context, []string) (func(context.Context) error, error) {
	return func(context.Context) error { g.released++; return nil }, nil
}

func TestGoogleV2PreflightAndMutationDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, method              string
		status                    int
		wantRequests, wantRelease int
	}{
		{"create validation", "create", 500, 0, 1},
		{"update read 404", "update", 404, 1, 1},
		{"delete read 404", "delete", 404, 1, 1},
		{"delete mutation 412", "delete-dispatched", 412, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: dispatchRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"code":%d,"message":"failure"}}`, tc.status))), Request: r}, nil
			})}
			svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
			if err != nil {
				t.Fatal(err)
			}
			provider := &Provider{svc: svc}
			guard := &recordingGuard{}
			_, err = calendar.GuardedWrite(t.Context(), guard, []string{"google:primary"}, func() (bool, error) {
				switch tc.method {
				case "create":
					_, err := provider.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{CalendarID: "primary", Notifications: "invalid"})
					return false, err
				case "update":
					_, err := provider.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}})
					return false, err
				case "delete":
					_, err := provider.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}, RequireNoAttendees: true, Notifications: calendar.NotificationsNone})
					return false, err
				default:
					_, err := provider.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "primary", EventID: "event"}})
					return false, err
				}
			}, func(v bool) bool { return v })
			if err == nil || requests != tc.wantRequests || guard.released != tc.wantRelease {
				t.Fatalf("err=%v requests=%d released=%d", err, requests, guard.released)
			}
			var api *calendar.APIError
			unknown := errors.As(err, &api) && api.Code == calendar.ErrorMutationOutcomeUnknown
			if unknown != (tc.wantRelease == 0) {
				t.Fatalf("unexpected outcome: %v", err)
			}
		})
	}
}

func TestGoogleOtherMutationPreflightAndDispatch(t *testing.T) {
	ref := calendar.EventRef{CalendarID: "primary", EventID: "event"}
	for _, tc := range []struct {
		name, operation, failure string
		wantCalls                int
		wantRelease              bool
	}{
		{"legacy update read failure", "legacy", "read", 1, true},
		{"legacy update write failure", "legacy", "write", 2, false},
		{"respond validation", "respond-invalid", "", 0, true},
		{"respond read failure", "respond", "read", 1, true},
		{"respond missing self", "respond", "no-self", 1, true},
		{"respond write failure", "respond", "write", 2, false},
		{"move validation", "move-invalid", "", 0, true},
		{"move read failure", "move", "read", 1, true},
		{"move nondefault", "move", "nondefault", 1, true},
		{"move write failure", "move", "write", 2, false},
		{"import validation", "import-invalid", "", 0, true},
		{"import write failure", "import", "write", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: dispatchRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				status := http.StatusOK
				body := `{"id":"event","eventType":"default","attendees":[{"self":true}]}`
				switch {
				case r.Method != http.MethodGet:
					return nil, errors.New("connection lost after dispatch")
				case tc.failure == "read":
					status = http.StatusNotFound
					body = `{"error":{"code":404,"message":"not found"}}`
				case tc.failure == "no-self":
					body = `{"id":"event","eventType":"default","attendees":[]}`
				case tc.failure == "nondefault":
					body = `{"id":"event","eventType":"outOfOffice"}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
			if err != nil {
				t.Fatal(err)
			}
			provider := &Provider{svc: svc}
			guard := &recordingGuard{}
			_, err = calendar.GuardedWrite(t.Context(), guard, []string{"google:primary"}, func() (bool, error) {
				switch tc.operation {
				case "legacy":
					_, e := provider.UpdateEvent(t.Context(), "primary", "event", calendar.EventUpdate{})
					return false, e
				case "respond-invalid":
					_, e := provider.RespondToEventV2(t.Context(), calendar.RespondToEventRequestV2{Ref: ref, Response: "invalid"})
					return false, e
				case "respond":
					_, e := provider.RespondToEventV2(t.Context(), calendar.RespondToEventRequestV2{Ref: ref, Response: "accepted"})
					return false, e
				case "move-invalid":
					_, e := provider.MoveEventV2(t.Context(), calendar.MoveEventRequestV2{Ref: ref})
					return false, e
				case "move":
					_, e := provider.MoveEventV2(t.Context(), calendar.MoveEventRequestV2{Ref: ref, DestinationCalendarID: "other"})
					return false, e
				case "import-invalid":
					_, e := provider.ImportEventV2(t.Context(), calendar.ImportEventRequestV2{CalendarID: "primary"})
					return false, e
				default:
					_, e := provider.ImportEventV2(t.Context(), calendar.ImportEventRequestV2{CalendarID: "primary", Event: calendar.EventCreateV2{ICalUID: "uid", Start: calendar.EventTime{Date: "2026-11-01"}, End: calendar.EventTime{Date: "2026-11-02"}}})
					return false, e
				}
			}, func(v bool) bool { return v })
			if err == nil || calls != tc.wantCalls || (guard.released == 1) != tc.wantRelease {
				t.Fatalf("err=%v calls=%d releases=%d", err, calls, guard.released)
			}
			var api *calendar.APIError
			unknown := errors.As(err, &api) && api.Code == calendar.ErrorMutationOutcomeUnknown
			if unknown == tc.wantRelease {
				t.Fatalf("incorrect outcome: %v", err)
			}
		})
	}
}
