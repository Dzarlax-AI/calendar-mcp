package providers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
	"golang.org/x/oauth2"
)

func TestShortGoogleLegacyOwnerOnUpgradedStore(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	now := time.Now().UTC()
	if _, err = legacy.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	sqlFiles, err := filepath.Glob("../storage/migrations/sqlite/00[1-7]_*.sql")
	if err != nil || len(sqlFiles) != 7 {
		t.Fatalf("pre-008 migrations: %v, %v", sqlFiles, err)
	}
	for i, filename := range sqlFiles {
		migration, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = legacy.ExecContext(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
		if _, err = legacy.ExecContext(ctx, "INSERT INTO schema_migrations VALUES (?, ?)", i+1, now); err != nil {
			t.Fatal(err)
		}
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := json.Marshal(&oauth2.Token{AccessToken: "legacy-access", RefreshToken: "legacy-refresh", Expiry: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.Encrypt(token, credentials.AssociatedData("legacy", "google"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.ExecContext(ctx, `INSERT INTO connections(id,provider,account_fingerprint,display_name,status,
		encrypted_credentials,credential_version,created_at,updated_at) VALUES('legacy','google','legacy','Synthetic','connected',?,1,?,?)`, ciphertext, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.ExecContext(ctx, `INSERT INTO calendars(id,connection_id,provider_calendar_id,name,can_read,can_write,supports_recurrence,discovered_at,google_primary)
		VALUES('google:legacy:cal','legacy','cal','Legacy calendar',true,true,true,?,true)`, now); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	api, err := storage.Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if err = api.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service := connections.New(api, cipher)
	factory := NewFactory(&config.Config{GoogleClientID: "synthetic", GoogleClientSecret: "synthetic"}, api, service)
	initial, err := factory.Build(ctx)
	if err != nil || len(initial) != 1 {
		t.Fatalf("legacy route construction: %d, %v", len(initial), err)
	}
	registry := calendar.NewRegistry(initial)
	registry.SetProviderRefresh(factory.RefreshProvider)
	registry.SetProviderDiscovery(factory.DiscoverGoogleRoutes)
	allowLegacy := false
	registry.SetGoogleOwnerLookup(func(ctx context.Context, raw string) (string, string, error) {
		return api.GoogleShortIDOwner(ctx, raw, allowLegacy)
	})
	if _, _, err = registry.ResolveContext(ctx, "google:primary"); err == nil {
		t.Fatal("read-model mode admitted an unverified legacy alias")
	}
	allowLegacy = true
	for _, raw := range []string{"primary", "cal"} {
		p, got, err := registry.ResolveContext(ctx, "google:"+raw)
		if err != nil || p == nil || calendar.ProviderRouteName(p) != "google@legacy" || got != raw {
			t.Fatalf("legacy alias %q: route=%v raw=%q err=%v", raw, p, got, err)
		}
	}
	if _, _, err = registry.ResolveContext(ctx, "google:unknown"); err == nil {
		t.Fatal("legacy account matched an unknown short ID")
	}
	if err = service.CreateOAuth(ctx, "other", "google", "Synthetic", &oauth2.Token{RefreshToken: "other-refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	other, err := api.BeginAuthorization(ctx, "other", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: "google:other:cal", ConnectionID: "other", ProviderCalendarID: "cal", Name: "Other", GooglePrimary: true}, AccessRole: "owner"}
	if err = api.CompleteGoogleAuthorization(ctx, other, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err = registry.ResolveContext(ctx, "google:primary"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("legacy and verified aliases must conflict: %v", err)
	}
}

func TestBootstrapMembershipAcrossStores(t *testing.T) {
	var calls atomic.Int32
	original := http.DefaultTransport
	http.DefaultTransport = runtimeRegistryTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host == "www.googleapis.com" && r.URL.Path == "/calendar/v3/users/me/calendarList" {
			return runtimeRegistryResponse(r, 200, `{"items":[{"id":"cal","summary":"Calendar","accessRole":"owner"}]}`), nil
		}
		return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
	})
	defer func() { http.DefaultTransport = original }()
	ctx := t.Context()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "state.db")
	api, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if err = api.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	worker, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc := connections.New(worker, cipher)
	now := time.Now().UTC()
	for _, id := range []string{"ready", "failed"} {
		if err := svc.CreateOAuth(ctx, id, "google", "Synthetic", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
		if err := worker.UpsertCalendar(ctx, storage.Calendar{ID: "google:" + id + ":cal", ConnectionID: id, ProviderCalendarID: "cal", Name: "Calendar", CanRead: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	ready, err := worker.BeginAuthorization(ctx, "ready", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := worker.BeginAuthorization(ctx, "failed", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	fac := NewFactory(&config.Config{GoogleClientID: "synthetic", GoogleClientSecret: "synthetic"}, api, connections.New(api, cipher))
	initial, err := fac.Build(ctx)
	if err != nil || len(initial) != 0 {
		t.Fatalf("initial routes %d: %v", len(initial), err)
	}
	reg := calendar.NewRegistry(initial)
	reg.SetProviderRefresh(fac.RefreshProvider)
	reg.SetProviderDiscovery(fac.DiscoverGoogleRoutes)
	if _, _, err := reg.ResolveContext(ctx, "google:ready:cal"); err == nil {
		t.Fatal("pending route available")
	}
	if err := worker.FailAuthorization(ctx, failed, now); err != nil {
		t.Fatal(err)
	}
	if err := worker.CompleteGoogleAuthorization(ctx, ready, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: "google:ready:cal", ConnectionID: "ready", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := reg.ResolveContext(ctx, "google:ready:cal"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, _, err := reg.ResolveContext(ctx, "google:failed:cal"); err == nil {
		t.Fatal("failed route available")
	}
	ps, err := reg.ProvidersContext(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("routes %d: %v", len(ps), err)
	}
	if calls.Load() != 0 {
		t.Fatalf("route registration made %d provider calls", calls.Load())
	}
	listed, err := reg.ListCalendars(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != "google:ready:cal" {
		t.Fatalf("list: %#v, %v", listed, err)
	}
	if _, raw, err := reg.Resolve("google:cal"); err != nil || raw != "cal" {
		t.Fatalf("legacy route %q: %v", raw, err)
	}
}

func TestBootstrapVerifiedEmptyAndFreeBusyRoutes(t *testing.T) {
	for _, role := range []string{"empty", "freeBusyReader"} {
		t.Run(role, func(t *testing.T) {
			ctx := t.Context()
			dsn := "sqlite://" + filepath.Join(t.TempDir(), "state.db")
			api, err := storage.Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			if err = api.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			worker, err := storage.Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close()
			cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{23}, 32)))
			if err != nil {
				t.Fatal(err)
			}
			svc := connections.New(worker, cipher)
			now := time.Now().UTC()
			for _, id := range []string{"ready", "pending"} {
				if err = svc.CreateOAuth(ctx, id, "google", "Synthetic", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
					t.Fatal(err)
				}
			}
			ready, err := worker.BeginAuthorization(ctx, "ready", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := worker.BeginAuthorization(ctx, "pending", nil, now); err != nil {
				t.Fatal(err)
			}
			fac := NewFactory(&config.Config{GoogleClientID: "synthetic", GoogleClientSecret: "synthetic"}, api, connections.New(api, cipher))
			initial, err := fac.Build(ctx)
			if err != nil || len(initial) != 0 {
				t.Fatalf("initial %d: %v", len(initial), err)
			}
			reg := calendar.NewRegistry(initial)
			reg.SetProviderRefresh(fac.RefreshProvider)
			reg.SetProviderDiscovery(fac.DiscoverGoogleRoutes)
			var discovered []storage.DiscoveredDetailCalendar
			if role != "empty" {
				discovered = []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: "google:ready:cal", ConnectionID: "ready", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: role}}
			}
			if err = worker.CompleteGoogleAuthorization(ctx, ready, discovered, now); err != nil {
				t.Fatal(err)
			}
			ps, err := reg.ProvidersContext(ctx)
			if err != nil || len(ps) != 1 {
				t.Fatalf("routes %d: %v", len(ps), err)
			}
			if _, raw, err := reg.ResolveContext(ctx, "google:ready:cal"); err != nil || raw != "cal" {
				t.Fatalf("ready route %q: %v", raw, err)
			}
			for _, id := range []string{"unknown", "pending"} {
				if _, _, err := reg.ResolveContext(ctx, "google:"+id+":cal"); err == nil {
					t.Fatalf("%s routed to ready account", id)
				}
			}
		})
	}
}

func TestShortGoogleOwnersFollowCompletedDiscoveryAcrossStores(t *testing.T) {
	ctx := t.Context()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "state.db")
	api, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if err = api.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	worker, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc := connections.New(worker, cipher)
	now := time.Now().UTC()
	for _, id := range []string{"a", "b"} {
		if err = svc.CreateOAuth(ctx, id, "google", id, &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(id string, entries []storage.DiscoveredDetailCalendar) {
		t.Helper()
		snap, e := worker.BeginAuthorization(ctx, id, nil, now)
		if e != nil {
			t.Fatal(e)
		}
		if e = worker.CompleteGoogleAuthorization(ctx, snap, entries, now); e != nil {
			t.Fatal(e)
		}
	}
	entry := func(id, raw string, primary bool) storage.DiscoveredDetailCalendar {
		return storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: "google:" + id + ":" + raw, ConnectionID: id, ProviderCalendarID: raw, Name: raw, GooglePrimary: primary}, AccessRole: "freeBusyReader"}
	}
	publish("a", []storage.DiscoveredDetailCalendar{entry("a", "shared", true)})
	f := NewFactory(&config.Config{GoogleClientID: "synthetic", GoogleClientSecret: "synthetic"}, api, connections.New(api, cipher))
	reg := calendar.NewRegistry(nil, calendar.RegistryOptions{Selection: api})
	reg.SetProviderDiscovery(f.DiscoverGoogleRoutes)
	reg.SetProviderRefresh(f.RefreshProvider)
	reg.SetGoogleOwnerLookup(func(ctx context.Context, raw string) (string, string, error) {
		return api.GoogleShortIDOwner(ctx, raw, false)
	})
	if _, raw, e := reg.ResolveContext(ctx, "google:primary"); e != nil || raw != "primary" {
		t.Fatalf("primary: %q %v", raw, e)
	}
	if _, raw, e := reg.ResolveEnabled(ctx, "google:primary"); e != nil || raw != "shared" {
		t.Fatalf("primary selection did not resolve the canonical calendar: %q %v", raw, e)
	}
	if e := api.SetCalendarEnabled(ctx, "google:a:shared", false, now); e != nil {
		t.Fatal(e)
	}
	if _, _, e := reg.ResolveEnabled(ctx, "google:primary"); e == nil {
		t.Fatal("primary alias bypassed the disabled calendar selection")
	}
	publish("b", []storage.DiscoveredDetailCalendar{entry("b", "shared", false)})
	if _, _, e := reg.ResolveContext(ctx, "google:shared"); e == nil || !strings.Contains(e.Error(), "ambiguous") {
		t.Fatalf("collision: %v", e)
	}
	publish("a", nil)
	if p, _, e := reg.ResolveContext(ctx, "google:shared"); e != nil || calendar.ProviderRouteName(p) != "google@b" {
		t.Fatalf("stale replacement: %v %v", p, e)
	}
	if _, _, e := reg.ResolveContext(ctx, "google:primary"); e == nil {
		t.Fatal("empty account retained primary")
	}
	// Replace the selected owner's authority exactly between the metadata
	// lookup and the route refresh, without dispatching any provider call.
	reg.SetGoogleOwnerLookup(func(ctx context.Context, raw string) (string, string, error) {
		owner, revision, err := api.GoogleShortIDOwner(ctx, raw, false)
		if err == nil && owner != "" {
			publish(owner, nil)
		}
		return owner, revision, err
	})
	if p, _, e := reg.ResolveContext(ctx, "google:shared"); p != nil || e == nil || !strings.Contains(e.Error(), "authority changed") {
		t.Fatalf("short ID resolved under a different account revision: %v, %v", p, e)
	}
}
