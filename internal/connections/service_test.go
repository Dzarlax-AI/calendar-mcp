package connections

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

func newTestService(t *testing.T) (*Service, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return New(store, cipher), store
}

func TestOAuthConnectionEncryptsAndRefreshesToken(t *testing.T) {
	ctx := context.Background()
	service, store := newTestService(t)
	original := &oauth2.Token{RefreshToken: "refresh-secret", AccessToken: "access-secret", Expiry: time.Now().UTC().Add(time.Hour)}
	if err := service.CreateOAuth(ctx, "google-1", "google", "Personal Google", original, []string{"calendar"}); err != nil {
		t.Fatal(err)
	}

	record, err := store.ConnectionByID(ctx, "google-1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(record.EncryptedCredentials, []byte("refresh-secret")) {
		t.Fatal("stored credentials contain plaintext token")
	}
	loaded, err := service.OAuthTokenStore("google-1", "google").Load()
	if err != nil || loaded.RefreshToken != original.RefreshToken {
		t.Fatalf("loaded = %#v, err = %v", loaded, err)
	}

	updated := &oauth2.Token{RefreshToken: "new-refresh", AccessToken: "new-access"}
	if err := service.OAuthTokenStore("google-1", "google").Save(updated); err != nil {
		t.Fatal(err)
	}
	loaded, err = service.OAuthTokenStore("google-1", "google").Load()
	if err != nil || loaded.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed token = %#v, err = %v", loaded, err)
	}

	if err := service.CreateOAuth(ctx, "google-2", "google", "Second Google", original, nil); err != nil {
		t.Fatalf("second connection for provider: %v", err)
	}
	if _, err := service.OAuthTokenStore("google-1", "microsoft").Load(); err == nil {
		t.Fatal("provider mismatch decrypted")
	}
}

func TestOAuthTokenRefreshPreservesVerifiedConnection(t *testing.T) {
	ctx := context.Background()
	service, store := newTestService(t)
	verifiedAt := time.Now().UTC().Add(-time.Minute)
	if err := service.CreateOAuth(ctx, "google-verified", "google", "Verified Google", &oauth2.Token{RefreshToken: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateConnectionVerification(ctx, "google-verified", "connected", "", verifiedAt); err != nil {
		t.Fatal(err)
	}
	if err := service.OAuthTokenStore("google-verified", "google").Save(&oauth2.Token{AccessToken: "refreshed", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	record, err := store.ConnectionByID(ctx, "google-verified")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "connected" || record.LastErrorCode != "" || record.LastVerifiedAt == nil || !record.LastVerifiedAt.Equal(verifiedAt) {
		t.Fatalf("refreshed connection = %#v", record)
	}
}

func TestReconnectOAuthPreservesExistingRefreshToken(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	expiry := time.Now().UTC().Add(time.Hour)
	if err := service.CreateOAuth(ctx, "google-1", "google", "Personal Google", &oauth2.Token{
		AccessToken: "old-access", RefreshToken: "existing-refresh",
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := service.ReconnectOAuth(ctx, "google-1", "google", &oauth2.Token{
		AccessToken: "new-access", Expiry: expiry,
	}); err != nil {
		t.Fatal(err)
	}

	loaded, err := service.OAuthTokenStore("google-1", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccessToken != "new-access" || loaded.RefreshToken != "existing-refresh" || !loaded.Expiry.Equal(expiry) {
		t.Fatalf("reconnected token = %#v", loaded)
	}
}

func TestAppleConnectionRoundTrip(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if err := service.CreateApple(ctx, "apple-1", "iCloud", "user@example.com", "app-password"); err != nil {
		t.Fatal(err)
	}
	got, err := service.LoadApple(ctx, "apple-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "user@example.com" || got.AppPassword != "app-password" {
		t.Fatalf("credentials = %#v", got)
	}
	if err := service.CreateApple(ctx, "apple-2", "iCloud", "", ""); err == nil {
		t.Fatal("empty Apple credentials succeeded")
	}
	if err := service.ReconnectApple(ctx, "apple-1", "", "new-password"); err == nil {
		t.Fatal("reconnect accepted an empty Apple username")
	}
	if err := service.ReconnectApple(ctx, "apple-1", "user@example.com", "new-password"); err != nil {
		t.Fatal(err)
	}
	record, err := store.ConnectionByID(ctx, "apple-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "pending" || record.LastVerifiedAt != nil {
		t.Fatalf("reconnected record = %#v", record)
	}
}

func TestVerifyDiscoversCalendarsWithPrefixedIDs(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if err := service.CreateOAuth(ctx, "google-1", "google", "Google", &oauth2.Token{RefreshToken: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{name: "google", calendars: []calendar.Calendar{{ID: "primary", Name: "Primary"}}}
	if err := service.VerifyAndDiscover(ctx, "google-1", provider); err != nil {
		t.Fatal(err)
	}
	discovered, err := store.ListCalendars(ctx, "google-1")
	if err != nil || len(discovered) != 1 || discovered[0].ID != "google:google-1:primary" {
		t.Fatalf("calendars = %#v, err = %v", discovered, err)
	}
	if err := service.Delete(ctx, "google-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConnectionByID(ctx, "google-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted connection error = %v", err)
	}
}

func TestVerifyRejectsDuplicateProviderAccountWithoutDeletingConnection(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	provider := &fakeProvider{name: "google", calendars: []calendar.Calendar{{ID: "primary", Name: "Primary"}}}
	for _, id := range []string{"google-1", "google-2"} {
		if err := service.CreateOAuth(ctx, id, "google", "Google", &oauth2.Token{RefreshToken: id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.VerifyAndDiscover(ctx, "google-1", provider); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyAndDiscover(ctx, "google-2", provider); err == nil {
		t.Fatal("duplicate provider account verified")
	}
	if _, err := store.ConnectionByID(ctx, "google-2"); err != nil {
		t.Fatalf("duplicate connection was deleted: %v", err)
	}
}

func TestVerifyRecordsErrorWhenCapabilityDiscoveryFails(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if err := service.CreateOAuth(ctx, "google-1", "google", "Google", &oauth2.Token{RefreshToken: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{name: "google", calendars: []calendar.Calendar{{ID: "primary", Name: "Primary"}}, capabilitiesErr: errors.New("capabilities unavailable")}
	if err := service.VerifyAndDiscover(ctx, "google-1", provider); err == nil {
		t.Fatal("verification succeeded despite capability failure")
	}
	record, err := store.ConnectionByID(ctx, "google-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "error" || record.LastErrorCode != "provider_discovery_failed" {
		t.Fatalf("connection status = %#v", record)
	}
}

type fakeProvider struct {
	name            string
	calendars       []calendar.Calendar
	capabilitiesErr error
}

func (p *fakeProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{}, p.capabilitiesErr
}

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	return p.calendars, nil
}
func (p *fakeProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (p *fakeProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) DeleteEvent(context.Context, string, string) error {
	return errors.New("not implemented")
}
