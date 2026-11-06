package apple

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

func TestApplePreflightDispatchBoundary(t *testing.T) {
	ref := calendar.EventRef{CalendarID: "/calendar/", EventID: "event.ics"}
	for _, tc := range []struct {
		name                      string
		call                      func(*Provider) error
		wantRequests, wantRelease int
	}{
		{"create validation", func(p *Provider) error {
			_, err := p.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{Event: calendar.EventCreateV2{Attendees: []calendar.AttendeeV2{{Email: "synthetic@example.invalid"}}}})
			return err
		}, 0, 1},
		{"update unsupported", func(p *Provider) error {
			_, err := p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, Scope: calendar.ScopeSeries, ExpectedETag: "synthetic"})
			return err
		}, 0, 1},
		{"delete invalid instance", func(p *Provider) error {
			_, err := p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Scope: calendar.ScopeSingle})
			return err
		}, 0, 1},
		{"update read failed", func(p *Provider) error {
			_, err := p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, Scope: calendar.ScopeSeries})
			return err
		}, 1, 1},
		{"legacy update read failed", func(p *Provider) error {
			_, err := p.UpdateEvent(t.Context(), ref.CalendarID, ref.EventID, calendar.EventUpdate{})
			return err
		}, 1, 1},
		{"legacy invalid delete path", func(p *Provider) error {
			return p.DeleteEvent(t.Context(), "/calendar/", "https://other.invalid/event.ics")
		}, 0, 1},
		{"delete mutation failed", func(p *Provider) error {
			_, err := p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Scope: calendar.ScopeSeries})
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
			p := eventSyncProvider(t, server)
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
