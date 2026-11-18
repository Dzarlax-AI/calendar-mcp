package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifiedCallerUsesAcceptedCredentialAndConfiguration(t *testing.T) {
	var callers []VerifiedCaller
	for _, input := range []struct {
		opts               Options
		credential, header string
	}{
		{Options{APIKey: "primary", LegacyAPIKey: "legacy"}, "primary", "Authorization"},
		{Options{APIKey: "primary", LegacyAPIKey: "legacy"}, "primary", "X-API-Key"},
		{Options{APIKey: "primary", LegacyAPIKey: "legacy"}, "legacy", "Authorization"},
		{Options{APIKey: "replacement", LegacyAPIKey: "legacy"}, "legacy", "Authorization"},
	} {
		h := Middleware(input.opts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, ok := VerifiedCallerFromContext(r.Context())
			if !ok {
				t.Fatal("accepted request has no verified caller")
			}
			callers = append(callers, caller)
			w.WriteHeader(http.StatusNoContent)
		}))
		r := httptest.NewRequest(http.MethodPost, "/mcp/modern", nil)
		value := input.credential
		if input.header == "Authorization" {
			value = "Bearer " + value
		}
		r.Header.Set(input.header, value)
		r.Header.Set("X-User-ID", "spoofed-user")
		r.Header.Set("X-Caller-Fingerprint", "spoofed-fingerprint")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status %d", rr.Code)
		}
	}
	if callers[0] != callers[1] {
		t.Fatal("same accepted key changed identity by header")
	}
	if callers[0].KeyFingerprint() == callers[2].KeyFingerprint() {
		t.Fatal("primary and legacy keys share identity")
	}
	if callers[2].KeyFingerprint() != callers[3].KeyFingerprint() || callers[2].ConfigurationFingerprint() == callers[3].ConfigurationFingerprint() {
		t.Fatal("rotation did not preserve key identity while changing configuration binding")
	}
}

func TestVerifiedCallerAbsentWithoutSuccessfulKeyCheck(t *testing.T) {
	if _, ok := VerifiedCallerFromContext(context.Background()); ok {
		t.Fatal("bare context trusted")
	}
	for _, opts := range []Options{{}, {APIKey: "primary"}, {AllowUnauthenticated: true}} {
		called := false
		h := Middleware(opts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if _, ok := VerifiedCallerFromContext(r.Context()); ok {
				t.Fatal("unauthenticated request has caller")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("X-User-ID", "primary")
		r.Header.Set("X-API-Key", "wrong")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if called != opts.AllowUnauthenticated {
			t.Fatalf("unexpected protected handler call: %v", called)
		}
	}
}
