package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/oauthflow"
	"calendar-mcp/internal/providers"
	"calendar-mcp/internal/storage"
	"golang.org/x/oauth2"
)

type runtimeCallbackTransport func(*http.Request) (*http.Response, error)

func (f runtimeCallbackTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func runtimeCallbackResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestRuntimeGoogleReconnectCallbackRequiresOwnRefreshToken(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "callback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{93}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc := connections.New(store, cipher)
	if err = svc.CreateOAuth(ctx, "a", "google", "A", &oauth2.Token{AccessToken: "a-original", RefreshToken: "a-refresh", Expiry: now.Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "a", "cal")
	entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: id, ConnectionID: "a", ProviderCalendarID: "cal", Name: "Synthetic"}, AccessRole: "owner"}
	snap, err := store.BeginAuthorization(ctx, "a", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	beforeTicket, err := store.CaptureWriterTicket(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "event", CalendarID: id, Provider: "google", Title: "Cached synthetic event", Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	if err = store.UpsertCachedEvent(ctx, event, now); err != nil {
		t.Fatal(err)
	}
	before, err := store.ConnectionByID(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListCalendars(ctx, "a")
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources=%v err=%v", sources, err)
	}
	cached, _, err := store.ListCachedEvents(ctx, []string{id}, now, now.Add(3*time.Hour))
	if err != nil || len(cached) != 1 {
		t.Fatalf("initial cache=%v err=%v", cached, err)
	}
	configOAuth := &oauth2.Config{ClientID: "synthetic", ClientSecret: "synthetic", RedirectURL: "https://calendar.example/oauth/google/callback", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/authorize", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}}
	flow := oauthflow.New(store, cipher, map[string]oauthflow.Provider{"google": oauthflow.ConfigProvider{Config: configOAuth}})
	fac := providers.NewFactory(&config.Config{GoogleClientID: "synthetic", GoogleClientSecret: "synthetic"}, store, svc)
	existing, err := fac.Build(ctx)
	if err != nil || len(existing) != 1 {
		t.Fatalf("providers=%d err=%v", len(existing), err)
	}
	oldProvider := existing[0]
	registry := calendar.NewRegistry(existing)
	registry.SetProviderRefresh(fac.RefreshProvider)
	builds, refreshes, exchanges := 0, 0, 0
	srv, err := New(store, svc, flow, fac.Build, Config{PublicURL: "https://calendar.example", TrustForwardAuth: true, BuildGoogleVerification: func(s storage.CredentialSnapshot) (calendar.Provider, error) {
		builds++
		return fac.BuildGoogleVerification(s)
	}, OnProvidersChanged: func([]calendar.Provider) { refreshes++ }})
	if err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	http.DefaultTransport = runtimeCallbackTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("code_verifier") == "" {
				return nil, fmt.Errorf("missing PKCE verifier")
			}
			exchanges++
			switch r.Form.Get("code") {
			case "candidate-b":
				return runtimeCallbackResponse(r, `{"access_token":"b-access","token_type":"Bearer","expires_in":3600}`), nil
			case "same-account":
				return runtimeCallbackResponse(r, `{"access_token":"a-new","refresh_token":"a-new-refresh","token_type":"Bearer","expires_in":3600}`), nil
			default:
				return nil, fmt.Errorf("unexpected code")
			}
		case "www.googleapis.com":
			if r.Header.Get("Authorization") != "Bearer a-new" || !strings.Contains(r.URL.Path, "/users/me/calendarList") {
				return nil, fmt.Errorf("unexpected synthetic Google request")
			}
			return runtimeCallbackResponse(r, `{"items":[{"id":"cal","summary":"Synthetic","accessRole":"owner","primary":true}]}`), nil
		default:
			return nil, fmt.Errorf("blocked synthetic host")
		}
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	callback := func(code string) *httptest.ResponseRecorder {
		t.Helper()
		start, e := flow.BeginReconnect(ctx, "google", "a", "/connections")
		if e != nil {
			t.Fatal(e)
		}
		q := url.Values{"state": {start.State}, "code": {code}}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://calendar.example/oauth/google/callback?"+q.Encode(), nil))
		return w
	}
	rejected := callback("candidate-b")
	if rejected.Code != http.StatusSeeOther || rejected.Header().Get("Location") != "/connections?status=verification_failed" {
		t.Fatalf("rejection %d %q", rejected.Code, rejected.Header().Get("Location"))
	}
	if builds != 0 || refreshes != 0 || exchanges != 1 {
		t.Fatalf("premature verification builds=%d refreshes=%d exchanges=%d", builds, refreshes, exchanges)
	}
	after, err := store.ConnectionByID(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	afterSources, err := store.ListCalendars(ctx, "a")
	if err != nil || len(afterSources) != 1 {
		t.Fatalf("sources=%v err=%v", afterSources, err)
	}
	afterCache, _, err := store.ListCachedEvents(ctx, []string{id}, now, now.Add(3*time.Hour))
	if err != nil || len(afterCache) != 1 || afterCache[0].Title != event.Title {
		t.Fatalf("cached event changed: %v %v", afterCache, err)
	}
	if !bytes.Equal(before.EncryptedCredentials, after.EncryptedCredentials) || before.AuthorizationRevision != after.AuthorizationRevision || before.Status != after.Status || !before.UpdatedAt.Equal(after.UpdatedAt) || sources[0].DetailReceipt != afterSources[0].DetailReceipt || !afterSources[0].CanRead {
		t.Fatal("rejected callback changed working grant")
	}
	accepted := callback("same-account")
	if accepted.Code != http.StatusSeeOther || accepted.Header().Get("Location") != "/connections?status=connected" {
		t.Fatalf("acceptance %d %q", accepted.Code, accepted.Header().Get("Location"))
	}
	if builds != 1 || refreshes != 1 || exchanges != 2 {
		t.Fatalf("successful callback builds=%d refreshes=%d exchanges=%d", builds, refreshes, exchanges)
	}
	token, err := svc.OAuthTokenStore("a", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "a-new" || token.RefreshToken != "a-new-refresh" {
		t.Fatal("new consent not persisted")
	}
	current, err := store.ConnectionByID(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	newSources, err := store.ListCalendars(ctx, "a")
	if err != nil || len(newSources) != 1 || !newSources[0].CanRead || newSources[0].DetailReceipt == sources[0].DetailReceipt || current.AuthorizationRevision == before.AuthorizationRevision || current.Status != "connected" {
		t.Fatalf("successful consent authority not established: %v", err)
	}
	currentTicket, err := store.CaptureWriterTicket(ctx, id)
	if err != nil || currentTicket.Generation != beforeTicket.Generation+1 {
		t.Fatalf("reconnect must advance the sync generation once, before=%d after=%d err=%v", beforeTicket.Generation, currentTicket.Generation, err)
	}
	p, _, err := registry.ResolveContext(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p == oldProvider {
		t.Fatal("existing registry did not refresh provider")
	}
	if _, err = p.ListCalendars(ctx); err != nil {
		t.Fatal(err)
	}
}
