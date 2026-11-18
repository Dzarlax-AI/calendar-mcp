package connections

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

func TestGoogleReconnectRequiresOwnRefreshToken(t *testing.T) {
	ctx := context.Background()
	service, store := newTestService(t)
	if err := service.CreateOAuth(ctx, "google-1", "google", "Personal Google", &oauth2.Token{AccessToken: "old-access", RefreshToken: "existing-refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	before, err := store.ConnectionByID(ctx, "google-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconnectOAuth(ctx, "google-1", "google", &oauth2.Token{AccessToken: "new-access"}); err == nil {
		t.Fatal("missing refresh token accepted")
	}
	after, err := store.ConnectionByID(ctx, "google-1")
	if err != nil {
		t.Fatal(err)
	}
	if before.AuthorizationRevision != after.AuthorizationRevision || !bytes.Equal(before.EncryptedCredentials, after.EncryptedCredentials) {
		t.Fatal("rejected reconnect changed credentials or revision")
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
	if err := verifyTestGoogle(ctx, service, "google-1", provider); err != nil {
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
	if err := verifyTestGoogle(ctx, service, "google-1", provider); err != nil {
		t.Fatal(err)
	}
	if err := verifyTestGoogle(ctx, service, "google-2", provider); err == nil {
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
	if err := verifyTestGoogle(ctx, service, "google-1", provider); err == nil {
		t.Fatal("verification succeeded despite capability failure")
	}
	record, err := store.ConnectionByID(ctx, "google-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "error" || record.LastErrorCode != "provider_verification_failed" {
		t.Fatalf("connection status = %#v", record)
	}
}

type fakeProvider struct {
	name            string
	calendars       []calendar.Calendar
	capabilitiesErr error
}

func verifyTestGoogle(ctx context.Context, service *Service, id string, p *fakeProvider) error {
	return service.VerifyGoogle(ctx, id, func(storage.CredentialSnapshot) (calendar.Provider, error) { return p, nil })
}
func (p *fakeProvider) DiscoverDetailCalendars(context.Context) ([]calendar.DetailDiscoveryEntry, error) {
	if p.capabilitiesErr != nil {
		return nil, p.capabilitiesErr
	}
	entries := make([]calendar.DetailDiscoveryEntry, 0, len(p.calendars))
	for _, c := range p.calendars {
		role := "owner"
		if c.ReadOnly {
			role = "reader"
		}
		entries = append(entries, calendar.DetailDiscoveryEntry{Calendar: c, AccessRole: role})
	}
	return entries, nil
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

func TestGooglePrimaryDiscoveryAndFailure(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if err := service.CreateOAuth(ctx, "google-1", "google", "Google", &oauth2.Token{RefreshToken: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{name: "google", calendars: []calendar.Calendar{{ID: "owner@example.test", Primary: true}, {ID: "shared@example.test"}}}
	check := func(want string) {
		t.Helper()
		got, err := store.ListCalendars(ctx, "google-1")
		if err != nil {
			t.Fatal(err)
		}
		found := ""
		for _, c := range got {
			if c.GooglePrimary {
				if found != "" {
					t.Fatal("multiple primary flags")
				}
				found = c.ProviderCalendarID
			}
		}
		if found != want {
			t.Fatalf("primary = %q, want %q", found, want)
		}
	}
	if err := verifyTestGoogle(ctx, service, "google-1", provider); err != nil {
		t.Fatal(err)
	}
	check("owner@example.test")
	provider.calendars[1].Primary = true
	if err := verifyTestGoogle(ctx, service, "google-1", provider); err != nil {
		t.Fatal(err)
	}
	check("")
	provider.calendars[1].Primary = false
	if err := verifyTestGoogle(ctx, service, "google-1", provider); err != nil {
		t.Fatal(err)
	}
	check("owner@example.test")
	provider.capabilitiesErr = errors.New("unavailable")
	if err := verifyTestGoogle(ctx, service, "google-1", provider); err == nil {
		t.Fatal("expected failure")
	}
	check("")
}

func TestGoogleDiscoveryPreservesAndUpdatesTimezone(t *testing.T) {
	service, store := newTestService(t)
	ctx := t.Context()
	if err := service.CreateOAuth(ctx, "timezone-account", "google", "Synthetic", &oauth2.Token{RefreshToken: "synthetic"}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{name: "google", calendars: []calendar.Calendar{{ID: "owner@example.test", Primary: true, Timezone: "Europe/Belgrade"}}}
	check := func(want string) {
		t.Helper()
		calendars, err := store.ListCalendars(ctx, "timezone-account")
		if err != nil || len(calendars) != 1 || calendars[0].Timezone != want {
			t.Fatalf("timezone want %s: %v", want, err)
		}
	}
	for _, step := range []struct{ input, want string }{{"Europe/Belgrade", "Europe/Belgrade"}, {"", "Europe/Belgrade"}, {"Local", "Europe/Belgrade"}, {"not-a-zone", "Europe/Belgrade"}, {"America/Los_Angeles", "America/Los_Angeles"}} {
		provider.calendars[0].Timezone = step.input
		if err := verifyTestGoogle(ctx, service, "timezone-account", provider); err != nil {
			t.Fatal(err)
		}
		check(step.want)
	}
}

func TestSnapshotTokenStoreRejectsSupersededRefresh(t *testing.T) {
	ctx := context.Background()
	service, store := newTestService(t)
	if err := service.CreateOAuth(ctx, "g", "google", "Google", &oauth2.Token{AccessToken: "old", RefreshToken: "old-refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.CredentialSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	bound := service.SnapshotOAuthTokenStore(snap)
	old, err := bound.Load()
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ConnectionByID(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if err = bound.Save(old); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.ConnectionByID(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.EncryptedCredentials, unchanged.EncryptedCredentials) || !before.UpdatedAt.Equal(unchanged.UpdatedAt) {
		t.Fatal("no-op save changed stored token")
	}
	if err = service.ReconnectOAuth(ctx, "g", "google", &oauth2.Token{AccessToken: "new", RefreshToken: "new-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err = bound.Save(old); !errors.Is(err, storage.ErrAuthorityChanged) {
		t.Fatalf("stale no-op save = %v", err)
	}
	old.AccessToken = "delayed"
	if err = bound.Save(old); !errors.Is(err, storage.ErrAuthorityChanged) {
		t.Fatalf("stale refresh = %v", err)
	}
	actual, err := service.OAuthTokenStore("g", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if actual.AccessToken != "new" || actual.RefreshToken != "new-refresh" {
		t.Fatal("stale write replaced reconnect")
	}
}

func TestDeletedAndRecreatedConnectionRejectsOldTokenSource(t *testing.T) {
	ctx := context.Background()
	service, store := newTestService(t)
	if err := service.CreateOAuth(ctx, "same", "google", "Old", &oauth2.Token{AccessToken: "old", RefreshToken: "old-refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	old, err := store.CredentialSnapshot(ctx, "same")
	if err != nil {
		t.Fatal(err)
	}
	bound := service.SnapshotOAuthTokenStore(old)
	token, err := bound.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Delete(ctx, "same"); err != nil {
		t.Fatal(err)
	}
	if err = service.CreateOAuth(ctx, "same", "google", "New", &oauth2.Token{AccessToken: "new", RefreshToken: "new-refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	token.AccessToken = "delayed"
	if err = bound.Save(token); !errors.Is(err, storage.ErrAuthorityChanged) {
		t.Fatalf("stale save=%v", err)
	}
	current, err := store.CredentialSnapshot(ctx, "same")
	if err != nil {
		t.Fatal(err)
	}
	if old.AuthorizationRevision == current.AuthorizationRevision {
		t.Fatal("reused authorization revision")
	}
}

type cancelDiscoveryProvider struct {
	fakeProvider
	cancel context.CancelFunc
}

func (p *cancelDiscoveryProvider) DiscoverDetailCalendars(ctx context.Context) ([]calendar.DetailDiscoveryEntry, error) {
	p.cancel()
	return nil, ctx.Err()
}

func TestCanceledGoogleVerificationClosesAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, store := newTestService(t)
	if err := service.CreateOAuth(ctx, "google-cancel", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic"}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &cancelDiscoveryProvider{fakeProvider: fakeProvider{name: "google"}, cancel: cancel}
	if err := service.VerifyGoogle(ctx, "google-cancel", func(storage.CredentialSnapshot) (calendar.Provider, error) { return provider, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("verify=%v", err)
	}
	record, err := store.ConnectionByID(context.Background(), "google-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "error" {
		t.Fatalf("status=%s", record.Status)
	}
	candidate, err := store.BeginOneLegacyGoogleAuthorization(context.Background(), time.Now().UTC())
	if err != nil || candidate != nil {
		t.Fatalf("canceled attempt retried: %v %v", candidate, err)
	}
}

func TestNonGoogleReconnectFencesVerificationAndRefresh(t *testing.T) {
	for _, kind := range []string{"apple", "microsoft"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			svc, store := newTestService(t)
			if kind == "apple" {
				if err := svc.CreateApple(ctx, "account", "Account", "old@example.test", "old"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := svc.CreateOAuth(ctx, "account", kind, "Account", &oauth2.Token{AccessToken: "old", RefreshToken: "refresh"}, nil); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.CredentialSnapshot(ctx, "account")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "microsoft" {
				if err = svc.OAuthTokenStore("account", kind).Save(&oauth2.Token{AccessToken: "refreshed", RefreshToken: "refresh"}); err != nil {
					t.Fatal(err)
				}
				refreshed, e := store.CredentialSnapshot(ctx, "account")
				if e != nil || refreshed.AuthorizationRevision != before.AuthorizationRevision {
					t.Fatalf("refresh rotated authorization: %v", e)
				}
			}
			reconnect := func() error {
				if kind == "apple" {
					return svc.ReconnectApple(ctx, "account", "new@example.test", "new")
				}
				return svc.ReconnectOAuth(ctx, "account", kind, &oauth2.Token{AccessToken: "new", RefreshToken: "new-refresh"})
			}
			if err = reconnect(); err != nil {
				t.Fatal(err)
			}
			pending, err := store.CredentialSnapshot(ctx, "account")
			if err != nil || pending.Status != "pending" || pending.AuthorizationRevision == before.AuthorizationRevision {
				t.Fatalf("pending: %+v %v", pending, err)
			}
			if err = reconnect(); err != nil {
				t.Fatal(err)
			}
			p := &fakeProvider{name: kind, calendars: []calendar.Calendar{{ID: "old", Name: "Old"}}}
			if err = svc.verifyNonGoogleSnapshot(ctx, pending, p); !errors.Is(err, storage.ErrAuthorityChanged) {
				t.Fatalf("stale verification: %v", err)
			}
			record, err := store.ConnectionByID(ctx, "account")
			if err != nil || record.Status != "pending" {
				t.Fatalf("stale verification changed status: %+v %v", record, err)
			}
			calendars, err := store.ListCalendars(ctx, "account")
			if err != nil || len(calendars) != 0 {
				t.Fatalf("stale discovery published: %+v %v", calendars, err)
			}
		})
	}
}

func TestNewConnectionsStayPendingAndBuilderFailureCloses(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"apple", "microsoft"} {
		t.Run(kind, func(t *testing.T) {
			svc, store := newTestService(t)
			var id string
			var err error
			if kind == "apple" {
				id, err = svc.ConnectApple(ctx, "Apple", "user@example.test", "password")
			} else {
				id, err = svc.ConnectOAuth(ctx, kind, "Microsoft", &oauth2.Token{AccessToken: "a", RefreshToken: "r"}, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			record, err := store.ConnectionByID(ctx, id)
			if err != nil || record.Status != "pending" {
				t.Fatalf("new connection: %+v %v", record, err)
			}
			if err = svc.VerifyNonGoogle(ctx, id, func(storage.CredentialSnapshot) (calendar.Provider, error) {
				return nil, errors.New("synthetic build failure")
			}); err == nil {
				t.Fatal("expected builder failure")
			}
			record, err = store.ConnectionByID(ctx, id)
			if err != nil || record.Status != "error" {
				t.Fatalf("failed verification: %+v %v", record, err)
			}
		})
	}
}

func TestReconnectSnapshotSurvivesLaterAttempts(t *testing.T) {
	for _, kind := range []string{"apple", "microsoft", "google"} {
		for _, outcome := range []string{"success", "failure", "cancel"} {
			for _, newerFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/newerFirst=%t", kind, outcome, newerFirst), func(t *testing.T) {
					ctx := context.Background()
					svc, store := newTestService(t)
					var err error
					if kind == "apple" {
						err = svc.CreateApple(ctx, "account", "Account", "old@example.test", "old")
					} else {
						err = svc.CreateOAuth(ctx, "account", kind, "Account", &oauth2.Token{AccessToken: "old", RefreshToken: "refresh"}, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					reconnect := func(label string) storage.CredentialSnapshot {
						var snap storage.CredentialSnapshot
						var e error
						if kind == "apple" {
							snap, e = svc.ReconnectAppleSnapshot(ctx, "account", label+"@example.test", label)
						} else {
							snap, e = svc.ReconnectOAuthSnapshot(ctx, "account", kind, &oauth2.Token{AccessToken: label, RefreshToken: label})
						}
						if e != nil {
							t.Fatal(e)
						}
						return snap
					}
					old := reconnect("first")
					current := reconnect("second")
					if old.AuthorizationRevision == current.AuthorizationRevision {
						t.Fatal("reused revision")
					}
					verify := func(snap storage.CredentialSnapshot, result string) error {
						verifyCtx := ctx
						if result == "cancel" {
							var cancel context.CancelFunc
							verifyCtx, cancel = context.WithCancel(ctx)
							cancel()
						}
						build := func(got storage.CredentialSnapshot) (calendar.Provider, error) {
							if got.AuthorizationRevision != snap.AuthorizationRevision {
								t.Fatal("builder received another revision")
							}
							if result == "failure" {
								return nil, errors.New("synthetic failure")
							}
							if result == "cancel" {
								return nil, verifyCtx.Err()
							}
							return &fakeProvider{name: kind, calendars: []calendar.Calendar{{ID: "cal-" + result, Name: "Calendar"}}}, nil
						}
						if kind == "google" {
							return svc.VerifyGoogleSnapshot(verifyCtx, snap, build)
						}
						return svc.VerifyNonGoogleSnapshot(verifyCtx, snap, build)
					}
					runOld := func() {
						if e := verify(old, outcome); e == nil {
							t.Fatal("stale verification unexpectedly succeeded")
						}
					}
					runNew := func() {
						if e := verify(current, "success"); e != nil {
							t.Fatalf("new verification: %v", e)
						}
					}
					if newerFirst {
						runNew()
						runOld()
					} else {
						runOld()
						runNew()
					}
					record, e := store.ConnectionByID(ctx, "account")
					if e != nil || record.Status != "connected" || record.AuthorizationRevision != current.AuthorizationRevision {
						t.Fatalf("current authority lost: %+v %v", record, e)
					}
					cals, e := store.ListCalendars(ctx, "account")
					if e != nil || len(cals) != 1 || cals[0].ProviderCalendarID != "cal-success" {
						t.Fatalf("current projection: %+v %v", cals, e)
					}
				})
			}
		}
	}
}
