package restapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
)

func TestV2RoutesAreFeatureGated(t *testing.T) {
	reg := calendar.NewRegistry(nil)
	app := application.New(reg)

	disabled := New(reg, app, auth.Options{AllowUnauthenticated: true}, false).Handler()
	disabledResponse := httptest.NewRecorder()
	disabled.ServeHTTP(disabledResponse, httptest.NewRequest(http.MethodGet, "/api/v2/capabilities", nil))
	if disabledResponse.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d, want %d", disabledResponse.Code, http.StatusNotFound)
	}

	enabled := New(reg, app, auth.Options{AllowUnauthenticated: true}, true).Handler()
	enabledResponse := httptest.NewRecorder()
	enabled.ServeHTTP(enabledResponse, httptest.NewRequest(http.MethodGet, "/api/v2/capabilities", nil))
	if enabledResponse.Code != http.StatusBadRequest {
		t.Fatalf("enabled status = %d, want %d", enabledResponse.Code, http.StatusBadRequest)
	}
}

func TestAvailabilityRouteIsNewAndStrict(t *testing.T) {
	reg := calendar.NewRegistry(nil)
	app := application.New(reg)
	old := New(reg, app, auth.Options{AllowUnauthenticated: true}, false).Handler()
	res := httptest.NewRecorder()
	old.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v2/availability/events", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("disabled = %d", res.Code)
	}
	enabled := New(reg, app, auth.Options{AllowUnauthenticated: true}, true).Handler()
	for _, path := range []string{
		"/api/v2/availability/events?calendar_id=google@acct:cal&start=2026-11-05T20:00:00Z&end=2026-11-06T04:00:00Z",
		"/api/v2/availability/events?calendar_id=google@acct:cal&start=2026-11-05T20:00:00Z&end=2026-11-06T04:00:00Z&view=expanded&page_token=external",
	} {
		res = httptest.NewRecorder()
		enabled.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("unsafe parameters status=%d", res.Code)
		}
	}
}
