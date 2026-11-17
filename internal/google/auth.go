package google

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	googleOAuth "golang.org/x/oauth2/google"

	"calendar-mcp/internal/token"
)

const (
	scopeCalendarListReadOnly = "https://www.googleapis.com/auth/calendar.calendarlist.readonly"
	scopeCalendarEvents       = "https://www.googleapis.com/auth/calendar.events"
)

// OAuthScopes returns the least-privilege scopes required to discover calendars
// and read or mutate their events. Callers receive a fresh slice so the shared
// authorization contract cannot be changed by mutation.
func OAuthScopes() []string {
	return []string{scopeCalendarListReadOnly, scopeCalendarEvents}
}

func newOAuthConfig(clientID, clientSecret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     googleOAuth.Endpoint,
		Scopes:       OAuthScopes(),
	}
}

func newHTTPClient(store token.Store, cfg *oauth2.Config, initial *oauth2.Token) *http.Client {
	if saved, err := store.Load(); err == nil && (saved.AccessToken != "" || saved.RefreshToken != "") {
		initial = saved
	}
	ts := token.TokenSource(store, cfg, initial)
	client := oauth2.NewClient(context.Background(), ts)
	client.Timeout = 30 * time.Second
	return client
}

// newSnapshotHTTPClient requires successful decryption of the captured token;
// unlike the legacy file-token path it never falls back to an empty token.
func newSnapshotHTTPClient(store token.Store, cfg *oauth2.Config) (*http.Client, error) {
	initial, err := store.Load()
	if err != nil {
		return nil, err
	}
	if initial == nil || (initial.AccessToken == "" && initial.RefreshToken == "") {
		return nil, fmt.Errorf("snapshot has no OAuth credentials")
	}
	client := &http.Client{Transport: token.SnapshotTransport(store, cfg, initial), Timeout: 30 * time.Second}
	return client, nil
}
