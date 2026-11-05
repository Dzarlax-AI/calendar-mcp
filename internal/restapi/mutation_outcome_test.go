package restapi

import (
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMutationOutcomeUnknownIsNotRetryable(t *testing.T) {
	for _, err := range []error{calendar.ErrProviderMutationOutcomeUnknown, fmt.Errorf("private-context: %w", calendar.ErrProviderMutationOutcomeUnknown), &calendar.APIError{Code: calendar.ErrorPartialFailure, Message: "private-payload", Cause: calendar.ErrProviderMutationOutcomeUnknown}} {
		for _, legacy := range []bool{false, true} {
			r := httptest.NewRecorder()
			if legacy {
				writeLegacyMutationError(r, err)
			} else {
				writeAPIError(r, err)
			}
			body := r.Body.String()
			if r.Code != http.StatusConflict || !strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "Check the calendar") || !strings.Contains(body, `"retryable":false`) || strings.Contains(body, "private-") {
				t.Fatalf("legacy=%v status=%d body=%s", legacy, r.Code, body)
			}
		}
	}
}

func TestGuardPredispatchErrorTakesPriorityOverUnknownOutcome(t *testing.T) {
	guarded := calendar.NewProviderMutationProtectionError(fmt.Errorf("private-secret: %w", calendar.ErrProviderMutationOutcomeUnknown))
	for _, value := range []error{guarded, fmt.Errorf("private-wrapper: %w", guarded)} {
		w := httptest.NewRecorder()
		writeAPIError(w, value)
		body := w.Body.String()
		if w.Code != http.StatusBadGateway || strings.Contains(body, "private-") || strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "calendar mutation protection unavailable") {
			t.Fatalf("status=%d body=%s", w.Code, body)
		}
	}
}

type timeoutGuard struct{ begins, resolves int }
type timeoutTicket struct{ g *timeoutGuard }

func (g *timeoutGuard) BeginProviderMutation(context.Context, []string) (calendar.ProviderMutationIntent, error) {
	g.begins++
	return &timeoutTicket{g}, nil
}
func (t *timeoutTicket) ResolveProviderMutation(context.Context) error { t.g.resolves++; return nil }

type timeoutV2Provider struct {
	privacyProvider
	calls int
}

func (p *timeoutV2Provider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}, nil
}
func (p *timeoutV2Provider) ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	return calendar.Page[calendar.EventV2]{}, nil
}
func (p *timeoutV2Provider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	return nil, nil
}
func (p *timeoutV2Provider) CreateEventV2(context.Context, calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.calls++
	return nil, context.DeadlineExceeded
}
func (p *timeoutV2Provider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	return nil, nil
}
func (p *timeoutV2Provider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	return nil, nil
}

func TestGuardedCreateTimeoutThroughHTTPDoesNotRecommendRetry(t *testing.T) {
	p := &timeoutV2Provider{}
	guard := &timeoutGuard{}
	registry := calendar.NewRegistry([]calendar.Provider{p})
	app := application.New(registry, application.WithProviderMutationGuard(guard))
	handler := New(registry, app, auth.Options{AllowUnauthenticated: true}, true).Handler()
	body := `{"calendar_id":"google:primary","event":{"title":"synthetic-private-event","start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v2/events", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"retryable":false`) || !strings.Contains(w.Body.String(), string(calendar.ErrorMutationOutcomeUnknown)) || strings.Contains(w.Body.String(), "synthetic-private-event") || p.calls != 1 || guard.begins != 1 || guard.resolves != 0 {
		t.Fatalf("status=%d body=%s provider=%d begins=%d resolves=%d", w.Code, w.Body.String(), p.calls, guard.begins, guard.resolves)
	}
}
