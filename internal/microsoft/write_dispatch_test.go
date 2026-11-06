package microsoft

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"calendar-mcp/internal/calendar"
)

type dispatchTestGuard struct{ released int }

func (g *dispatchTestGuard) BeginWriteDispatch(context.Context, []string) (func(context.Context) error, error) {
	return func(context.Context) error { g.released++; return nil }, nil
}

func TestMicrosoftPreflightDispatchBoundary(t *testing.T) {
	ref := calendar.EventRef{CalendarID: "calendar", EventID: "event"}
	for _, tc := range []struct {
		name                      string
		call                      func(*Provider) error
		wantRequests, wantRelease int
	}{
		{"create validation", func(p *Provider) error {
			_, err := p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{RequireNoAttendees: true})
			return err
		}, 0, 1},
		{"update unsupported", func(p *Provider) error {
			_, err := p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, RequireNoAttendees: true})
			return err
		}, 0, 1},
		{"delete unsupported", func(p *Provider) error {
			_, err := p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, RequireNoAttendees: true})
			return err
		}, 0, 1},
		{"update read failed", func(p *Provider) error {
			_, err := p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, Scope: calendar.ScopeSeries})
			return err
		}, 1, 1},
		{"delete read failed", func(p *Provider) error {
			_, err := p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Notifications: calendar.NotificationsNone})
			return err
		}, 1, 1},
		{"legacy create attendees", func(p *Provider) error {
			_, err := p.CreateEvent(t.Context(), ref.CalendarID, calendar.EventCreate{Attendees: []calendar.Attendee{{Email: "synthetic@example.invalid"}}})
			return err
		}, 0, 1},
		{"legacy update attendees", func(p *Provider) error {
			attendees := []calendar.Attendee{}
			_, err := p.UpdateEvent(t.Context(), ref.CalendarID, ref.EventID, calendar.EventUpdate{Attendees: &attendees})
			return err
		}, 0, 1},
		{"delete mutation failed", func(p *Provider) error {
			_, err := p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Notifications: calendar.NotificationsAll})
			return err
		}, 1, 0},
		{"create mutation failed", func(p *Provider) error {
			_, err := p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{CalendarID: ref.CalendarID, Event: calendar.EventCreateV2{Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}})
			return err
		}, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				http.Error(w, "synthetic failure", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			p := &Provider{client: server.Client(), baseURL: server.URL}
			guard := &dispatchTestGuard{}
			_, err := calendar.GuardedWrite(t.Context(), guard, []string{ref.CalendarID}, func() (bool, error) { return false, tc.call(p) }, func(bool) bool { return false })
			if err == nil || requests != tc.wantRequests || guard.released != tc.wantRelease {
				t.Fatalf("err=%v requests=%d releases=%d", err, requests, guard.released)
			}
			if tc.wantRelease == 0 && !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) {
				t.Fatalf("mutation ambiguity lost: %v", err)
			}
		})
	}
}
