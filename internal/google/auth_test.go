package google

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"golang.org/x/oauth2"
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

func TestNewHTTPClientUsesSavedAccessTokenWithoutRefreshToken(t *testing.T) {
	saved := &oauth2.Token{AccessToken: "saved-access", Expiry: time.Now().Add(time.Hour)}
	store := &staticTokenStore{token: saved}
	client := newHTTPClient(store, newOAuthConfig("client", "secret"), &oauth2.Token{})
	transport, ok := client.Transport.(*oauth2.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.Transport)
	}
	transport.Base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer saved-access" {
			t.Errorf("Authorization = %q", got)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
	})

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://calendar.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type staticTokenStore struct {
	token *oauth2.Token
}

func (s *staticTokenStore) Load() (*oauth2.Token, error) { return s.token, nil }
func (s *staticTokenStore) Save(token *oauth2.Token) error {
	s.token = token
	return nil
}
