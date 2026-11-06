package restapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
)

type deniedWriteFence struct{}

func (deniedWriteFence) BeginWriteDispatch(context.Context, []string) (func(context.Context) error, error) {
	return nil, errors.New("synthetic busy")
}

type fencedRESTProvider struct {
	calendar.EventProviderV2
	writes int
}

func (*fencedRESTProvider) Name() string { return "google" }
func (p *fencedRESTProvider) DeleteEvent(context.Context, string, string) error {
	p.writes++
	return nil
}
func (p *fencedRESTProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}, nil
}
func (p *fencedRESTProvider) CreateEventV2(context.Context, calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.writes++
	return &calendar.EventV2{ID: "synthetic"}, nil
}
func TestRESTLegacyAndV2WritesHonorFence(t *testing.T) {
	p := &fencedRESTProvider{}
	reg := calendar.NewRegistry([]calendar.Provider{p}, calendar.RegistryOptions{WriteGuard: deniedWriteFence{}})
	handler := New(reg, application.New(reg), auth.Options{AllowUnauthenticated: true}, true).Handler()
	cases := []struct{ method, path, body string }{
		{http.MethodDelete, "/api/events?calendar_id=google:primary&event_id=event-1", ""},
		{http.MethodPost, "/api/v2/events", `{"calendar_id":"google:primary","event":{"title":"synthetic","start":{"date":"2026-10-26"},"end":{"date":"2026-10-27"}}}`},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code < 400 || p.writes != 0 {
			t.Fatalf("%s: status=%d writes=%d", tc.path, w.Code, p.writes)
		}
	}
}
