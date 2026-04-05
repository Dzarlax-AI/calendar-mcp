package google

import (
	"net/http"

	"golang.org/x/oauth2"
	googleOAuth "golang.org/x/oauth2/google"

	"calendar-mcp/internal/token"
)

const (
	scopeCalendar = "https://www.googleapis.com/auth/calendar"
)

func newOAuthConfig(clientID, clientSecret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     googleOAuth.Endpoint,
		Scopes:       []string{scopeCalendar},
	}
}

func newHTTPClient(store *token.FileStore, cfg *oauth2.Config, refreshToken string) *http.Client {
	initial := &oauth2.Token{RefreshToken: refreshToken}
	if saved, err := store.Load(); err == nil && saved.RefreshToken != "" {
		initial = saved
	}
	ts := store.TokenSource(cfg, initial)
	return oauth2.NewClient(nil, ts)
}
