package microsoft

import (
	"net/http"

	"golang.org/x/oauth2"

	"calendar-mcp/internal/token"
)

func azureEndpoint(tenantID string) oauth2.Endpoint {
	base := "https://login.microsoftonline.com/" + tenantID + "/oauth2/v2.0"
	return oauth2.Endpoint{
		AuthURL:  base + "/authorize",
		TokenURL: base + "/token",
	}
}

func newOAuthConfig(clientID, clientSecret, tenantID string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     azureEndpoint(tenantID),
		Scopes:       []string{"https://graph.microsoft.com/Calendars.ReadWrite", "offline_access"},
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
