package mcpserver

import (
	"calendar-mcp/internal/calendar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type privacyGuard struct{ err error }

func (g privacyGuard) BeginProviderMutation(context.Context, []string) (calendar.ProviderMutationIntent, error) {
	return nil, g.err
}

type privacyProvider struct{}

func (privacyProvider) Name() string                                               { return "google" }
func (privacyProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) { return nil, nil }
func (privacyProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (privacyProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	panic("unexpected provider mutation")
}
func (privacyProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	panic("unexpected provider mutation")
}
func (privacyProvider) DeleteEvent(context.Context, string, string) error {
	panic("unexpected provider mutation")
}

func TestLegacyMutationGuardErrorPreservesCauseWithoutLeaking(t *testing.T) {
	secret := errors.New("synthetic-secret-private-payload")
	r := calendar.NewRegistry([]calendar.Provider{privacyProvider{}}, calendar.RegistryOptions{MutationGuard: privacyGuard{err: secret}})
	_, err := r.CreateEvent(t.Context(), "google:primary", calendar.EventCreate{})
	if !errors.Is(err, secret) {
		t.Fatal("cause is not available for internal recovery")
	}
	for _, value := range []error{err, fmt.Errorf("request failed: %w", err)} {
		result := legacyMutationError(value)
		data, _ := json.Marshal(result)
		body := string(data)
		if !result.IsError || strings.Contains(body, "synthetic-secret-private-payload") || !strings.Contains(body, "calendar mutation protection unavailable") {
			t.Fatalf("unsafe response %s", body)
		}
	}
}

func TestLegacyPredispatchGuardErrorIsNotReportedAsDispatched(t *testing.T) {
	r := calendar.NewRegistry([]calendar.Provider{privacyProvider{}}, calendar.RegistryOptions{MutationGuard: privacyGuard{err: calendar.ErrProviderMutationOutcomeUnknown}})
	_, err := r.CreateEvent(t.Context(), "google:primary", calendar.EventCreate{})
	result := legacyMutationError(err)
	data, _ := json.Marshal(result)
	body := string(data)
	if strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "calendar mutation protection unavailable") {
		t.Fatalf("wrong classification %s", body)
	}
}
