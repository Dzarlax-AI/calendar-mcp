package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
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

func TestMicrosoftVerifiedAccessOnlyTokenBuildsWorkingRoutes(t *testing.T) {
	ctx := t.Context()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "access-only.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc := connections.New(store, cipher)
	factory := NewFactory(&config.Config{MS365ClientID: "synthetic", MS365ClientSecret: "synthetic", MS365TenantID: "synthetic"}, store, svc)
	var requests []string
	transport := http.DefaultTransport
	http.DefaultTransport = runtimeRegistryTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.microsoft.com" || r.Header.Get("Authorization") != "Bearer access-only" {
			return nil, fmt.Errorf("unexpected synthetic Microsoft request to %s", r.URL.Host)
		}
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/v1.0/me/calendars":
			return runtimeRegistryResponse(r, 200, `{"value":[{"id":"cal","name":"Synthetic","canEdit":true}]}`), nil
		case "/v1.0/me/calendars/cal":
			return runtimeRegistryResponse(r, 200, `{"canEdit":true}`), nil
		case "/v1.0/me/calendars/cal/calendarView":
			return runtimeRegistryResponse(r, 200, `{"value":[]}`), nil
		case "/v1.0/me/calendars/cal/calendarView/delta":
			return runtimeRegistryResponse(r, 200, `{"value":[],"@odata.deltaLink":"https://graph.microsoft.com/v1.0/me/calendars/cal/calendarView/delta?cursor=synthetic"}`), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	defer func() { http.DefaultTransport = transport }()
	now := time.Now().UTC()
	id, err := svc.ConnectOAuth(ctx, "microsoft", "Synthetic", &oauth2.Token{AccessToken: "access-only", Expiry: now.Add(time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyNonGoogle(ctx, id, factory.BuildNonGoogleVerification); err != nil {
		t.Fatalf("verification: %v", err)
	}
	for _, broken := range []struct {
		id         string
		ciphertext []byte
	}{{"bad-microsoft", []byte("malformed synthetic credentials")}, {"oversized-microsoft", make([]byte, 16385)}} {
		if err = store.CreateConnection(ctx, storage.Connection{ID: broken.id, Provider: "microsoft", DisplayName: "Synthetic",
			Status: "connected", EncryptedCredentials: broken.ciphertext, CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := factory.Build(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("build = %d routes, %v", len(routes), err)
	}
	registry := calendar.NewRegistry(routes)
	registry.SetProviderRefresh(factory.RefreshProvider)
	if _, err = registry.GetEvents(ctx, "microsoft:"+id+":cal", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("live read: %v", err)
	}
	workerRoutes, err := factory.Build(ctx)
	if err != nil || len(workerRoutes) != 1 {
		t.Fatalf("worker build = %d routes, %v", len(workerRoutes), err)
	}
	worker, ok := workerRoutes[0].(calendar.EventSyncProvider)
	if !ok {
		t.Fatal("worker route has no delta reader")
	}
	page, err := worker.SyncEvents(ctx, calendar.EventSyncRequest{
		CalendarID: "cal", Mode: calendar.EventSyncReplacement,
		Window: calendar.EventSyncWindow{Start: now, End: now.Add(time.Hour)},
	})
	if err != nil || !page.Complete || page.NextCursor == "" {
		t.Fatalf("worker delta read: %+v, %v", page, err)
	}
	current, err := svc.ReconnectOAuthSnapshot(ctx, id, "microsoft", &oauth2.Token{AccessToken: "expired", Expiry: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.VerifyNonGoogleSnapshot(ctx, current, factory.BuildNonGoogleVerification); err == nil || !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("expired token verification error = %v", err)
	}
	if got := strings.Join(requests, ","); got != "/v1.0/me/calendars,/v1.0/me/calendars/cal,/v1.0/me/calendars/cal/calendarView,/v1.0/me/calendars/cal/calendarView/delta" {
		t.Fatalf("Graph requests = %s", got)
	}
}

func TestMicrosoftRegistryRefreshFollowsWorkerRotation(t *testing.T) {
	ctx := t.Context()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "microsoft.db")
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
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service := connections.New(worker, cipher)
	for _, fixture := range []struct {
		id, access string
		expiry     time.Time
	}{{"ms", "expired", now.Add(-time.Hour)}, {"other", "other-access", now.Add(time.Hour)}} {
		value := &oauth2.Token{AccessToken: fixture.access, RefreshToken: fixture.id + "-refresh", Expiry: fixture.expiry}
		if err := service.CreateOAuth(ctx, fixture.id, "microsoft", "Synthetic", value, nil); err != nil {
			t.Fatal(err)
		}
	}
	var refreshes, writes atomic.Int32
	transport := http.DefaultTransport
	http.DefaultTransport = runtimeRegistryTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "login.microsoftonline.com":
			if refreshes.Add(1) != 1 {
				return nil, fmt.Errorf("unexpected repeated Microsoft OAuth refresh")
			}
			return runtimeRegistryResponse(r, 200, `{"access_token":"worker-access","token_type":"Bearer","expires_in":3600,"refresh_token":"worker-refresh"}`), nil
		case "graph.microsoft.com":
			access := r.Header.Get("Authorization")
			switch r.URL.Path {
			case "/v1.0/me/calendars":
				if access == "Bearer worker-access" || access == "Bearer other-access" {
					return runtimeRegistryResponse(r, 200, `{"value":[{"id":"cal"}]}`), nil
				}
			case "/v1.0/me/calendars/cal/events":
				if r.Method == http.MethodPost && access == "Bearer worker-access" {
					writes.Add(1)
					// Rotate while a mutation is already in flight.
					snapshot, err := worker.CredentialSnapshot(ctx, "ms")
					if err != nil {
						return nil, err
					}
					value := &oauth2.Token{AccessToken: "next-access", RefreshToken: "next-refresh", Expiry: now.Add(time.Hour)}
					if err = service.SnapshotOAuthTokenStore(snapshot).Save(value); err != nil {
						return nil, err
					}
					return runtimeRegistryResponse(r, 201, `{"id":"created"}`), nil
				}
			case "/v1.0/me/calendars/cal/calendarView":
				if access == "Bearer next-access" {
					return runtimeRegistryResponse(r, 200, `{"value":[]}`), nil
				}
			}
		}
		return nil, fmt.Errorf("unexpected synthetic Microsoft request %s %s", r.Method, r.URL.Path)
	})
	defer func() { http.DefaultTransport = transport }()

	cfg := &config.Config{MS365ClientID: "synthetic", MS365ClientSecret: "synthetic", MS365TenantID: "synthetic"}
	apiFactory := NewFactory(cfg, api, connections.New(api, cipher))
	apiProviders, err := apiFactory.Build(ctx)
	if err != nil || len(apiProviders) != 2 {
		t.Fatalf("API providers: %d, %v", len(apiProviders), err)
	}
	workerProviders, err := NewFactory(cfg, worker, service).Build(ctx)
	if err != nil || len(workerProviders) != 2 {
		t.Fatalf("worker providers: %d, %v", len(workerProviders), err)
	}
	for _, p := range workerProviders {
		if calendar.ProviderRouteName(p) == "microsoft@ms" {
			if _, err := p.ListCalendars(ctx); err != nil {
				t.Fatalf("worker refresh: %v", err)
			}
		}
	}
	registry := calendar.NewRegistry(apiProviders)
	registry.SetProviderRefresh(apiFactory.RefreshProvider)
	unrelated, _, err := registry.Resolve("microsoft:other:cal")
	if err != nil {
		t.Fatal(err)
	}
	if calendars, err := registry.ListCalendars(ctx); err != nil || len(calendars) != 2 {
		t.Fatalf("API list after worker refresh: %v, %v", calendars, err)
	}
	event := calendar.EventCreate{Title: "Synthetic", Start: now, End: now.Add(time.Hour)}
	if created, err := registry.CreateEvent(ctx, "microsoft:ms:cal", event); err != nil || created.ID != "microsoft:created" {
		t.Fatalf("API write overlapping worker refresh: %v, %v", created, err)
	}
	if _, err := registry.GetEvents(ctx, "microsoft:ms:cal", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("API read after second rotation: %v", err)
	}
	if current, _, err := registry.Resolve("microsoft:other:cal"); err != nil || current != unrelated {
		t.Fatalf("unrelated route changed: %v", err)
	}
	if refreshes.Load() != 1 || writes.Load() != 1 {
		t.Fatalf("OAuth refreshes = %d, Graph writes = %d", refreshes.Load(), writes.Load())
	}
	current, _, err := registry.Resolve("microsoft:ms:cal")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.UpdateConnectionCredentials(ctx, "ms", []byte("invalid synthetic snapshot"), 999, now); err != nil {
		t.Fatal(err)
	}
	if err := worker.UpdateConnectionVerification(ctx, "ms", "connected", "", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ResolveContext(ctx, "microsoft:ms:cal"); err == nil || !strings.Contains(err.Error(), "credential snapshot mismatch") {
		t.Fatalf("invalid replacement reported success: %v", err)
	}
	if stillRegistered, _, err := registry.Resolve("microsoft:ms:cal"); err != nil || stillRegistered != current {
		t.Fatalf("invalid replacement altered registered route: %v", err)
	}
}

type runtimeRegistryTransport func(*http.Request) (*http.Response, error)

func (f runtimeRegistryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func runtimeRegistryResponse(r *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func runtimeRegistryWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic request timed out")
	}
}

type runtimeRegistryFixture struct {
	api, worker *storage.Store
	service     *connections.Service
	registry    *calendar.Registry
	entry       storage.DiscoveredDetailCalendar
}

func newRuntimeRegistryFixture(t *testing.T) runtimeRegistryFixture {
	t.Helper()
	ctx := context.Background()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "registry.db")
	api, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	if err = api.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	worker, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{67}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc := connections.New(worker, cipher)
	now := time.Now().UTC()
	entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: calendar.AccountCalendarID("google", "a", "cal"), ConnectionID: "a", ProviderCalendarID: "cal", Name: "Synthetic"}, AccessRole: "owner"}
	for _, id := range []string{"a", "b"} {
		if err = svc.CreateOAuth(ctx, id, "google", "Synthetic", &oauth2.Token{AccessToken: id + "-old", RefreshToken: id + "-refresh", Expiry: now.Add(time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
		snap, e := worker.BeginAuthorization(ctx, id, nil, now)
		if e != nil {
			t.Fatal(e)
		}
		item := entry
		item.Calendar.ID = calendar.AccountCalendarID("google", id, "cal")
		item.Calendar.ConnectionID = id
		if e = worker.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{item}, now); e != nil {
			t.Fatal(e)
		}
	}
	fac := NewFactory(&config.Config{GoogleClientID: "synthetic", GoogleClientSecret: "synthetic"}, api, connections.New(api, cipher))
	providers, err := fac.Build(ctx)
	if err != nil || len(providers) != 2 {
		t.Fatalf("build %d: %v", len(providers), err)
	}
	reg := calendar.NewRegistry(providers)
	reg.SetProviderRefresh(fac.RefreshProvider)
	return runtimeRegistryFixture{api, worker, svc, reg, entry}
}
func (f runtimeRegistryFixture) reconnect(t *testing.T, access string, complete bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.service.ReconnectOAuth(ctx, "a", "google", &oauth2.Token{AccessToken: access, RefreshToken: access + "-refresh", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if complete {
		snap, err := f.worker.CredentialSnapshot(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if err = f.worker.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{f.entry}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRuntimeRegistryCrossStoreRefresh(t *testing.T) {
	f := newRuntimeRegistryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	var mu sync.Mutex
	var headers []string
	old := http.DefaultTransport
	http.DefaultTransport = runtimeRegistryTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" {
			return nil, fmt.Errorf("blocked synthetic host")
		}
		mu.Lock()
		headers = append(headers, r.Header.Get("Authorization"))
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/users/me/calendarList") {
			return runtimeRegistryResponse(r, 200, `{"items":[{"id":"cal","summary":"Synthetic","accessRole":"owner"}]}`), nil
		}
		return runtimeRegistryResponse(r, 200, `{"accessRole":"owner","items":[]}`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	other, _, err := f.registry.ResolveContext(ctx, "google:b:cal")
	if err != nil {
		t.Fatal(err)
	}
	f.reconnect(t, "a-new", false)
	if _, _, err = f.registry.ResolveContext(ctx, "google:a:cal"); err == nil {
		t.Fatal("pending account was available")
	}
	if _, err = f.registry.ListCalendars(ctx); err == nil {
		t.Fatal("pending account listed")
	}
	snap, err := f.worker.CredentialSnapshot(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.worker.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{f.entry}, now); err != nil {
		t.Fatal(err)
	}
	if _, err = f.registry.ListCalendars(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.registry.GetEvents(ctx, "google:a:cal", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.registry.GetEvents(ctx, "", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p, raw, err := f.registry.ResolveContext(ctx, "google:a:cal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.(calendar.EventProviderV2).ListEventsV2(ctx, calendar.ListEventsRequestV2{CalendarID: raw, Start: now, End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	again, _, err := f.registry.ResolveContext(ctx, "google:b:cal")
	if err != nil || again != other {
		t.Fatal("other route changed")
	}
	mu.Lock()
	seenA, seenB := false, false
	for _, v := range headers {
		if v == "Bearer a-new" {
			seenA = true
		}
		if v == "Bearer b-old" {
			seenB = true
		}
		if v == "Bearer a-old" {
			t.Error("old account credential used after reconnect")
		}
	}
	mu.Unlock()
	if !seenA || !seenB {
		t.Fatal("expected both account credentials")
	}
	st := f.service.SnapshotOAuthTokenStore(snap)
	tok, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	tok.AccessToken = "a-cas"
	if err = st.Save(tok); err != nil {
		t.Fatal(err)
	}
	if _, err = f.registry.GetEvents(ctx, "google:a:cal", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	last := headers[len(headers)-1]
	mu.Unlock()
	if last != "Bearer a-cas" {
		t.Fatalf("CAS refresh header=%q", last)
	}

	failed, err := f.worker.BeginAuthorization(ctx, "a", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.worker.FailAuthorization(ctx, failed, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.registry.ResolveContext(ctx, "google:a:cal"); err == nil {
		t.Fatal("failed account was available")
	}
	if _, err = f.registry.GetEvents(ctx, "", now, now.Add(time.Hour)); err == nil {
		t.Fatal("fanout succeeded with failed account")
	}
	again, _, err = f.registry.ResolveContext(ctx, "google:b:cal")
	if err != nil || again != other {
		t.Fatal("failed account disturbed other route")
	}
}
func TestRuntimeRegistryDoesNotReplayMutationOrRelabelLateRead(t *testing.T) {
	f := newRuntimeRegistryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	mutations := 0
	old := http.DefaultTransport
	http.DefaultTransport = runtimeRegistryTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" {
			return nil, fmt.Errorf("blocked host")
		}
		if r.Method == http.MethodPost {
			if r.URL.Query().Get("sendUpdates") != "none" {
				return nil, fmt.Errorf("notifications not disabled")
			}
			mu.Lock()
			mutations++
			mu.Unlock()
			started <- struct{}{}
			<-release
			return runtimeRegistryResponse(r, 200, `{"id":"created"}`), nil
		}
		started <- struct{}{}
		<-release
		return runtimeRegistryResponse(r, 200, `{"accessRole":"owner","items":[]}`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	read := make(chan error, 1)
	write := make(chan error, 1)
	go func() { _, e := f.registry.GetEvents(ctx, "google:a:cal", now, now.Add(time.Hour)); read <- e }()
	go func() {
		_, e := f.registry.CreateEvent(ctx, "google:a:cal", calendar.EventCreate{Title: "Synthetic", Start: now, End: now.Add(time.Hour)})
		write <- e
	}()
	runtimeRegistryWait(t, started)
	runtimeRegistryWait(t, started)
	f.reconnect(t, "a-new", true)
	close(release)
	if err := <-read; err == nil {
		t.Fatal("late old read accepted under new authority")
	}
	_ = <-write
	mu.Lock()
	count := mutations
	mu.Unlock()
	if count != 1 {
		t.Fatalf("mutation requests=%d", count)
	}
}

func TestRuntimeRegistrySupersededCredentialDenialDoesNotCloseGrant(t *testing.T) {
	for _, kind := range []string{"invalid_grant", "api_401"} {
		t.Run(kind, func(t *testing.T) {
			f := newRuntimeRegistryFixture(t)
			ctx := context.Background()
			now := time.Now().UTC()
			snap, err := f.worker.CredentialSnapshot(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "invalid_grant" {
				st := f.service.SnapshotOAuthTokenStore(snap)
				tok, e := st.Load()
				if e != nil {
					t.Fatal(e)
				}
				tok.Expiry = now.Add(-time.Hour)
				if e = st.Save(tok); e != nil {
					t.Fatal(e)
				}
			}
			current, err := f.worker.CredentialSnapshot(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{}, 4)
			release := make(chan struct{})
			old := http.DefaultTransport
			http.DefaultTransport = runtimeRegistryTransport(func(r *http.Request) (*http.Response, error) {
				if kind == "invalid_grant" && r.URL.Host == "oauth2.googleapis.com" {
					started <- struct{}{}
					<-release
					return runtimeRegistryResponse(r, 400, `{"error":"invalid_grant"}`), nil
				}
				if kind == "api_401" && r.URL.Host == "www.googleapis.com" {
					started <- struct{}{}
					<-release
					return runtimeRegistryResponse(r, 401, `{}`), nil
				}
				return nil, fmt.Errorf("blocked synthetic host")
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			done := make(chan error, 1)
			go func() { _, e := f.registry.GetEvents(ctx, "google:a:cal", now, now.Add(time.Hour)); done <- e }()
			runtimeRegistryWait(t, started)
			st := f.service.SnapshotOAuthTokenStore(current)
			tok, err := st.Load()
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			tok.AccessToken = "winner"
			tok.RefreshToken = "winner-refresh"
			tok.Expiry = now.Add(time.Hour)
			if err = st.Save(tok); err != nil {
				close(release)
				t.Fatal(err)
			}
			close(release)
			if err = <-done; err == nil {
				t.Fatal("expected stale denial")
			}
			record, err := f.worker.ConnectionByID(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			sources, err := f.worker.ListCalendars(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != "connected" || record.AuthorizationRevision != current.AuthorizationRevision || len(sources) != 1 || !sources[0].CanRead {
				t.Fatal("superseded credential denial closed current grant")
			}
		})
	}
}
