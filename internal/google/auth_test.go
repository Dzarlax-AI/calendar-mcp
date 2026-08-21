package google

import (
	"slices"
	"testing"
)

func TestOAuthScopesAreLeastPrivilege(t *testing.T) {
	want := []string{
		"https://www.googleapis.com/auth/calendar.calendarlist.readonly",
		"https://www.googleapis.com/auth/calendar.events",
	}
	got := OAuthScopes()
	if !slices.Equal(got, want) {
		t.Fatalf("OAuthScopes() = %v, want %v", got, want)
	}
	if slices.Contains(got, "https://www.googleapis.com/auth/calendar") {
		t.Fatal("OAuthScopes() includes the broad calendar scope")
	}
}

func TestOAuthScopesReturnsDefensiveCopy(t *testing.T) {
	first := OAuthScopes()
	first[0] = "mutated"
	second := OAuthScopes()
	if second[0] != scopeCalendarListReadOnly {
		t.Fatalf("OAuthScopes() shared mutable state: %v", second)
	}
}

func TestNewOAuthConfigUsesCanonicalScopes(t *testing.T) {
	cfg := newOAuthConfig("client", "secret")
	if !slices.Equal(cfg.Scopes, OAuthScopes()) {
		t.Fatalf("newOAuthConfig scopes = %v, want %v", cfg.Scopes, OAuthScopes())
	}
}
