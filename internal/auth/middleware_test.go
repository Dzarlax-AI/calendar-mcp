package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareFailsClosedWithoutConfiguration(t *testing.T) {
	called := false
	h := Middleware(Options{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if called {
		t.Fatal("protected handler was called without authentication configuration")
	}
}

func TestMiddlewareAllowsExplicitUnauthenticatedMode(t *testing.T) {
	h := Middleware(Options{AllowUnauthenticated: true})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestMiddlewareAcceptsConfiguredCredentials(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
	}{
		{name: "primary bearer", header: "Authorization", value: "Bearer primary"},
		{name: "primary api key", header: "X-API-Key", value: "primary"},
		{name: "legacy bearer", header: "Authorization", value: "Bearer legacy"},
		{name: "legacy api key", header: "X-API-Key", value: "legacy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Middleware(Options{APIKey: "primary", LegacyAPIKey: "legacy"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			req.Header.Set(tt.header, tt.value)
			rr := httptest.NewRecorder()

			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
			}
		})
	}
}

func TestMiddlewareRejectsMissingCredentialWhenLegacyKeyIsEmpty(t *testing.T) {
	called := false
	h := Middleware(Options{APIKey: "primary"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if called {
		t.Fatal("protected handler was called without a credential")
	}
}

func TestMiddlewareRejectsWrongCredential(t *testing.T) {
	h := Middleware(Options{APIKey: "secret"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}
