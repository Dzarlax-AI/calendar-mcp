package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/nativeapi"
	"calendar-mcp/internal/providers"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/web"
	"golang.org/x/oauth2"
)

type numericSQLiteTransport func(*http.Request) (*http.Response, error)

func (f numericSQLiteTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNumericSQLiteWorkerBootstrapAndCachedEndpoints(t *testing.T) {
	for _, format := range []string{"unix", "unix_milli", "unix_micro", "unix_nano"} {
		for _, convert := range []string{"false", "true"} {
			t.Run(format+"/"+convert, func(t *testing.T) { testNumericSQLiteEntryPoints(t, format, convert) })
		}
	}
}

func testNumericSQLiteEntryPoints(t *testing.T, format, convert string) {
	t.Helper()
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "calendar.db") + "?_time_integer_format=" + format + "&_inttotime=" + convert
	store, err := storage.Open(ctx, dsn)
	check(err)
	defer store.Close()
	check(store.Migrate(ctx))
	check(store.CheckSchema(ctx))
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	check(err)
	now := time.Now().UTC().Truncate(time.Second)
	early := time.Date(1970, 1, 3, 0, 0, 0, 0, time.UTC)
	connectionService := connections.New(store, cipher)
	check(connectionService.CreateOAuth(ctx, "account", "microsoft", "Synthetic",
		&oauth2.Token{AccessToken: "synthetic-access", Expiry: now.Add(time.Hour)}, nil))
	check(store.UpdateConnectionVerification(ctx, "account", "connected", "", early))
	const source = "microsoft:account:a-source"
	const target = "microsoft:account:z-target"
	for _, item := range []struct{ id, raw string }{{source, "a-source"}, {target, "z-target"}} {
		check(store.UpsertCalendar(ctx, storage.Calendar{ID: item.id, ConnectionID: "account", ProviderCalendarID: item.raw,
			Name: item.raw, CanRead: true, CanWrite: true, DiscoveredAt: early}))
	}
	calendars, err := store.ListAllCalendars(ctx)
	check(err)
	if len(calendars) != 2 || !calendars[0].DiscoveredAt.Equal(early) {
		t.Fatalf("calendar metadata: %+v", calendars)
	}
	finished := early.Add(time.Minute)
	check(store.CreateRule(ctx, storage.Rule{ID: "rule", SourceCalendarID: source, TargetCalendarID: target, State: "paused",
		IntervalSeconds: 600, LookaheadDays: 1, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: early, UpdatedAt: early}))
	check(store.EnqueueJob(ctx, storage.Job{ID: "job", RuleID: "rule", Kind: "dry_run", State: "succeeded", AvailableAt: early, CreatedAt: early, FinishedAt: &finished}))
	check(store.StartRun(ctx, storage.Run{ID: "run", JobID: "job", RuleID: "rule", Trigger: "dry_run", Outcome: "succeeded", StartedAt: early, FinishedAt: &finished, DryRun: true}))

	cfg := &config.Config{MS365ClientID: "synthetic", MS365ClientSecret: "synthetic", MS365TenantID: "synthetic",
		EventReadModelEnabled: true, EventCacheLookbackDays: 1, EventCacheLookaheadDays: 2, EventSyncMicrosoftInterval: time.Minute}
	factory := providers.NewFactory(cfg, store, connectionService)
	routes, err := factory.Build(ctx)
	check(err)
	if len(routes) != 1 || calendar.ProviderRouteName(routes[0]) != "microsoft@account" {
		t.Fatalf("startup routes: %v", routes)
	}

	eventStart, eventEnd := now, now.Add(time.Minute)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var workerCalls, calls int
	http.DefaultTransport = numericSQLiteTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Host != "graph.microsoft.com" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			return nil, fmt.Errorf("unexpected synthetic provider call")
		}
		body := `{"canEdit":true}`
		switch r.URL.Path {
		case "/v1.0/me/calendars/a-source/calendarView/delta":
			workerCalls++
			const graphDateTime = "2006-01-02T15:04:05"
			body = fmt.Sprintf(`{"value":[{"id":"worker-event","subject":"Synthetic delta","type":"singleInstance","start":{"dateTime":%q,"timeZone":"UTC"},"end":{"dateTime":%q,"timeZone":"UTC"}}],"@odata.deltaLink":"https://graph.microsoft.com/v1.0/me/calendars/a-source/calendarView/delta?cursor=synthetic"}`, eventStart.Format(graphDateTime), eventEnd.Format(graphDateTime))
		case "/v1.0/me/calendars/a-source", "/v1.0/me/calendars/z-target":
		default:
			return nil, fmt.Errorf("unexpected synthetic Graph path %q", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	check(runOneEventSync(ctx, store, factory, cfg, now))
	if workerCalls != 1 {
		t.Fatalf("worker delta calls: %d", workerCalls)
	}

	app := application.New(calendar.NewRegistry(routes))
	window := eventReadModelWindow(cfg, now)
	webServer, err := web.New(store, connectionService, nil, factory.Build, web.Config{PublicURL: "https://calendar.example",
		TrustForwardAuth: true, ApplicationService: app, EventReadModelEnabled: &cfg.EventReadModelEnabled, EventReadModelWindow: window})
	check(err)
	native := nativeapi.New(nativeapi.Config{App: app, CachedEventsApp: app.CloneWithEventReadModel(store, window), Store: store, ReadOnlyToken: "synthetic-readonly"})
	get := func(handler http.Handler, path string) []byte {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		req.Header.Set("X-authentik-username", "synthetic")
		req.Header.Set("Authorization", "Bearer synthetic-readonly")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, res.Code, res.Body.String())
		}
		return res.Body.Bytes()
	}
	var bootstrap struct {
		Rules []struct {
			CreatedAt time.Time `json:"created_at"`
		}
		Runs []struct {
			StartedAt time.Time `json:"started_at"`
		}
	}
	check(json.Unmarshal(get(webServer.Handler(), "/api/ui/bootstrap"), &bootstrap))
	if len(bootstrap.Rules) != 1 || !bootstrap.Rules[0].CreatedAt.Equal(early) || len(bootstrap.Runs) != 1 || !bootstrap.Runs[0].StartedAt.Equal(early) {
		t.Fatalf("bootstrap metadata: %+v", bootstrap)
	}

	providerCalls := calls
	for _, offset := range []int{0, 2 * 60 * 60, -5*60*60 - 30*60} {
		zone := time.FixedZone("test", offset)
		q := url.Values{"start": {eventStart.In(zone).Format(time.RFC3339)}, "end": {eventEnd.In(zone).Format(time.RFC3339)}, "calendar_id": {source}}
		for _, body := range [][]byte{get(webServer.Handler(), "/api/ui/events?"+q.Encode()), get(native.Handler(), "/cached-events?"+q.Encode())} {
			var response struct {
				Items   []struct{ Title string }
				Sources []struct{ Status string }
			}
			check(json.Unmarshal(body, &response))
			if len(response.Items) != 1 || response.Items[0].Title != "Synthetic delta" || len(response.Sources) != 1 || response.Sources[0].Status != "ready" {
				t.Fatalf("cached response: %s", body)
			}
		}
	}
	if calls != providerCalls {
		t.Fatal("cached HTTP reads reached Graph")
	}
}
