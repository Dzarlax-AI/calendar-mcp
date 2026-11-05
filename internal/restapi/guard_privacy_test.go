package restapi

import (
	"calendar-mcp/internal/calendar"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		w := httptest.NewRecorder()
		writeLegacyMutationError(w, value)
		if strings.Contains(w.Body.String(), "synthetic-secret-private-payload") || !strings.Contains(w.Body.String(), "calendar mutation protection unavailable") {
			t.Fatalf("unsafe response %s", w.Body.String())
		}
	}
}

func TestLegacyPredispatchGuardErrorIsNotReportedAsDispatched(t *testing.T) {
	r := calendar.NewRegistry([]calendar.Provider{privacyProvider{}}, calendar.RegistryOptions{MutationGuard: privacyGuard{err: calendar.ErrProviderMutationOutcomeUnknown}})
	_, err := r.CreateEvent(t.Context(), "google:primary", calendar.EventCreate{})
	w := httptest.NewRecorder()
	writeLegacyMutationError(w, err)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(w.Body.String(), "calendar mutation protection unavailable") {
		t.Fatalf("wrong classification %d %s", w.Code, w.Body.String())
	}
}
