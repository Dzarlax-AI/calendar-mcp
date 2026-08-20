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
