package mcpserver

import (
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestMutationOutcomeUnknownIsNotRetryable(t *testing.T) {
	for _, err := range []error{calendar.ErrProviderMutationOutcomeUnknown, fmt.Errorf("private-context: %w", calendar.ErrProviderMutationOutcomeUnknown), &calendar.APIError{Code: calendar.ErrorPartialFailure, Message: "private-payload", Cause: calendar.ErrProviderMutationOutcomeUnknown}} {
		result := v2Error(err)
		data, _ := json.Marshal(result)
		body := string(data)
		if !result.IsError || !strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "Check the calendar") || !strings.Contains(body, `"retryable":false`) || strings.Contains(body, "private-") {
			t.Fatalf("v2 body=%s", body)
		}
		result = legacyMutationError(err)
		data, _ = json.Marshal(result)
		body = string(data)
		if !result.IsError || !strings.Contains(body, "Check the calendar") || !strings.Contains(body, `"retryable":false`) || strings.Contains(body, "private-") {
			t.Fatalf("legacy body=%s", body)
		}
	}
}

func TestGuardPredispatchErrorTakesPriorityOverUnknownOutcome(t *testing.T) {
	guarded := calendar.NewProviderMutationProtectionError(fmt.Errorf("private-secret: %w", calendar.ErrProviderMutationOutcomeUnknown))
	for _, value := range []error{guarded, fmt.Errorf("private-wrapper: %w", guarded)} {
		for _, legacy := range []bool{false, true} {
			result := v2Error(value)
			if legacy {
				result = legacyMutationError(value)
			}
			data, _ := json.Marshal(result)
			body := string(data)
			if !result.IsError || strings.Contains(body, "private-") || strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "calendar mutation protection unavailable") {
				t.Fatalf("legacy=%v body=%s", legacy, body)
			}
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
	*catalogBehaviorProvider
	calls int
}

func (p *timeoutV2Provider) CreateEventV2(context.Context, calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.calls++
	return nil, context.DeadlineExceeded
}

func TestGuardedCreateTimeoutThroughMCPDoesNotRecommendRetry(t *testing.T) {
	p := &timeoutV2Provider{catalogBehaviorProvider: &catalogBehaviorProvider{name: "google", capabilities: calendar.CalendarCapabilities{NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}}}
	guard := &timeoutGuard{}
	registry := calendar.NewRegistry([]calendar.Provider{p})
	app := application.New(registry, application.WithProviderMutationGuard(guard))
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, registry, app, auth.Options{APIKey: "key"}, Options{EnableV2: true, CatalogMode: CatalogModern}); err != nil {
		t.Fatal(err)
	}
	endpoint := "http://catalog.test/mcp/modern"
	_, session := listHTTPTools(t, mux, endpoint, "key")
	result := callHTTPTool(t, mux, endpoint, "key", session, "create_event", map[string]any{"calendar_id": "google:primary", "event": map[string]any{"title": "synthetic-private-event", "start": map[string]any{"date": "2026-11-05"}, "end": map[string]any{"date": "2026-11-06"}}})
	raw, _ := json.Marshal(result)
	body := string(raw)
	if result["isError"] != true || !strings.Contains(body, `"retryable":false`) || !strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || strings.Contains(body, "synthetic-private-event") || p.calls != 1 || guard.begins != 1 || guard.resolves != 0 {
		t.Fatalf("body=%s provider=%d begins=%d resolves=%d", body, p.calls, guard.begins, guard.resolves)
	}
}
