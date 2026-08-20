package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"

	"golang.org/x/oauth2"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

func TestFactoryBuildsDatabaseBackedProviders(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	service := connections.New(store, cipher)
	if err := service.CreateOAuth(ctx, "google", "google", "Google", &oauth2.Token{RefreshToken: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.CreateOAuth(ctx, "google-2", "google", "Second Google", &oauth2.Token{RefreshToken: "refresh-2"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.CreateOAuth(ctx, "microsoft", "microsoft", "Microsoft", &oauth2.Token{RefreshToken: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.CreateApple(ctx, "apple", "Apple", "user@example.com", "password"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{GoogleClientID: "id", GoogleClientSecret: "secret", MS365ClientID: "id", MS365ClientSecret: "secret", MS365TenantID: "tenant", AppleCalDAVURL: "https://caldav.icloud.com"}
	got, err := NewFactory(cfg, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("providers = %d, want 4", len(got))
	}
	names := map[string]bool{}
	routes := map[string]bool{}
	for _, provider := range got {
		names[provider.Name()] = true
		routes[calendar.ProviderRouteName(provider)] = true
	}
	for _, name := range []string{"google", "microsoft", "apple"} {
		if !names[name] {
			t.Fatalf("provider %q not built", name)
		}
	}
	if !routes["google@google"] || !routes["google@google-2"] {
		t.Fatalf("account routes = %#v", routes)
	}
}

func TestFactorySkipsConnectedProvidersWithoutApplicationCredentials(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	service := connections.New(store, cipher)
	if err := service.CreateOAuth(ctx, "google", "google", "Google", &oauth2.Token{RefreshToken: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.CreateApple(ctx, "apple", "Apple", "user@example.com", "password"); err != nil {
		t.Fatal(err)
	}

	got, err := NewFactory(&config.Config{AppleCalDAVURL: "https://caldav.icloud.com"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name() != "apple" {
		t.Fatalf("providers = %#v, want only Apple", got)
	}
}
