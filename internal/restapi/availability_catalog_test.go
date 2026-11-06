package restapi

import (
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type emptyCatalogSelection struct{}

func (emptyCatalogSelection) CalendarEnabled(context.Context, string) (bool, error) {
	return false, nil
}
func (emptyCatalogSelection) ConfiguredProviderRoutes(context.Context, string) ([]string, error) {
	return []string{}, nil
}
func (emptyCatalogSelection) EnabledProviderCalendars(context.Context, string) ([]string, error) {
	return []string{}, nil
}
func (emptyCatalogSelection) AvailabilityCalendarSelection(context.Context, string) (bool, bool, error) {
	return false, false, nil
}
func TestAvailabilityCatalogAuthAndContract(t *testing.T) {
	reg := calendar.NewRegistry(nil, calendar.RegistryOptions{Selection: emptyCatalogSelection{}})
	app := application.New(reg)
	handler := New(reg, app, auth.Options{APIKey: "synthetic"}, true).Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/availability/calendars?provider=google", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated catalog accepted")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v2/availability/calendars?provider=google", nil)
	req.Header.Set("Authorization", "Bearer synthetic")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body struct {
		Provider  string              `json:"provider"`
		Complete  bool                `json:"complete"`
		Calendars []calendar.Calendar `json:"calendars"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Provider != "google" || !body.Complete || body.Calendars == nil || len(body.Calendars) != 0 {
		t.Fatal("invalid empty catalog")
	}
	for _, query := range []string{"", "?provider=unknown", "?provider=google&provider=apple", "?provider=google&other=x"} {
		req = httptest.NewRequest(http.MethodGet, "/api/v2/availability/calendars"+query, nil)
		req.Header.Set("Authorization", "Bearer synthetic")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid provider accepted")
		}
	}
}
