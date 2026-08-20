package google

import (
	"context"
	"net/http"
	"time"

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

func newHTTPClient(store token.Store, cfg *oauth2.Config, initial *oauth2.Token) *http.Client {
	if saved, err := store.Load(); err == nil && saved.RefreshToken != "" {
		initial = saved
	}
	ts := token.TokenSource(store, cfg, initial)
	client := oauth2.NewClient(context.Background(), ts)
	client.Timeout = 30 * time.Second
	return client
}
