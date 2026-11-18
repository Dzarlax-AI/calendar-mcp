package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/eventsync"
	"calendar-mcp/internal/storage"
	"golang.org/x/oauth2"
)

type syntheticTransport func(*http.Request) (*http.Response, error)

func (f syntheticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFactoryDenialObservationUsesOperationScope(t *testing.T) {
	cases := []struct {
		name               string
		status             int
		reason             string
		mutation, readable bool
	}{
		{"list404", 404, "", false, false},
		{"list401", 401, "", false, false},
		{"event404", 404, "", false, true},
		{"mutation403known", 403, "forbiddenForNonOrganizer", true, true},
		{"preflight403known", 403, "forbiddenForNonOrganizer", false, false},
		{"mutation403mixed", 403, "forbiddenForNonOrganizer,unknownReason", true, false},
		{"quota403mixed", 403, "rateLimitExceeded,unknownReason", false, false},
		{"mutation403unknown", 403, "unknownReason", true, false},
		{"quota403", 403, "rateLimitExceeded", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
			if err != nil {
				t.Fatal(err)
			}
			service := connections.New(store, cipher)
			if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
				t.Fatal(err)
			}
			snap, err := store.BeginAuthorization(ctx, "account", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			id := calendar.AccountCalendarID("google", "account", "cal")
			if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}}, now); err != nil {
				t.Fatal(err)
			}
			if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "www.googleapis.com" {
					return nil, fmt.Errorf("blocked synthetic host")
				}
				var entries []string
				for _, reason := range strings.Split(tc.reason, ",") {
					entries = append(entries, fmt.Sprintf(`{"reason":%q}`, reason))
				}
				body := `{"error":{"errors":[` + strings.Join(entries, ",") + `]}}`
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			defer func() { http.DefaultTransport = original }()
			providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
			if err != nil || len(providers) != 1 {
				t.Fatalf("build=%d %v", len(providers), err)
			}
			if tc.name == "preflight403known" {
				_, err = providers[0].UpdateEvent(ctx, "cal", "event", calendar.EventUpdate{})
			} else if tc.name == "event404" {
				v2 := providers[0].(calendar.EventProviderV2)
				_, err = v2.GetEventV2(ctx, calendar.EventRef{CalendarID: "cal", EventID: "event"})
			} else if tc.mutation {
				err = providers[0].DeleteEvent(ctx, "cal", "event")
			} else {
				_, err = providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour))
			}
			if err == nil {
				t.Fatal("expected provider denial")
			}
			calendars, err := store.ListCalendars(ctx, "account")
			if err != nil {
				t.Fatal(err)
			}
			if len(calendars) != 1 || calendars[0].CanRead != tc.readable {
				t.Fatalf("source=%+v", calendars)
			}
		})
	}
}

func TestOldProvider401CannotCloseReconnectedGrant(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "old", RefreshToken: "old", Expiry: now.Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}
	first, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, first, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" {
			return nil, fmt.Errorf("blocked synthetic host")
		}
		close(started)
		<-release
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); done <- e }()
	<-started
	if err = service.ReconnectOAuth(ctx, "account", "google", &oauth2.Token{AccessToken: "new", RefreshToken: "new", Expiry: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	second, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, second, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-done; err == nil {
		t.Fatal("expected old request error")
	}
	record, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "connected" || record.AuthorizationRevision != second.AuthorizationRevision || len(sources) != 1 || !sources[0].CanRead {
		t.Fatal("old 401 revoked new authorization")
	}
}

func TestFactoryTokenSourceInvalidGrantClosesCurrentAccount(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "expired", RefreshToken: "synthetic", Expiry: now.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "oauth2.googleapis.com" {
			return nil, fmt.Errorf("blocked synthetic host")
		}
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); err == nil {
		t.Fatal("expected token error")
	}
	record, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "error" || record.AuthorizationRevision == snap.AuthorizationRevision {
		t.Fatalf("account state=%s", record.Status)
	}
}

func TestFactoryTokenSourceRefreshPreservesAuthorization(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "expired", RefreshToken: "old-refresh", Expiry: now.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			body = `{"access_token":"fresh","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`
		case "www.googleapis.com":
			body = `{"accessRole":"owner","items":[]}`
		default:
			return nil, fmt.Errorf("blocked synthetic host")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("first refreshed request failed: %v", err)
	}
	token, err := service.OAuthTokenStore("account", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "fresh" || token.RefreshToken != "rotated" {
		t.Fatal("rotated token not stored")
	}
	current, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if current.AuthorizationRevision != snap.AuthorizationRevision || before[0].DetailReceipt != after[0].DetailReceipt || !after[0].CanRead {
		t.Fatal("ordinary refresh changed authorization")
	}
}

func TestActualGoogleSyncClosesOldPrivateProjectionOnRoleChange(t *testing.T) {
	for _, role := range []string{"reader", "writerWithoutPrivateAccess", ""} {
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
			service := connections.New(store, cipher)
			if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
				t.Fatal(err)
			}
			snap, err := store.BeginAuthorization(ctx, "account", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			id := calendar.AccountCalendarID("google", "account", "cal")
			entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}
			if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
				t.Fatal(err)
			}
			window := storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
			if err = store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
				t.Fatal(err)
			}
			ticket, err := store.CaptureWriterTicket(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			event := calendar.EventV2{ID: "old", CalendarID: id, Provider: "google", Title: "private old title", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
			if err = store.UpsertCachedEventWithTicket(ctx, ticket, event, now); err != nil {
				t.Fatal(err)
			}
			seed, err := store.ClaimDueCalendarSync(ctx, "seed", now, now.Add(time.Minute))
			if err != nil || seed == nil {
				t.Fatalf("seed claim=%v %v", seed, err)
			}
			nextSync := now
			if err = store.ApplyEventSyncPage(ctx, *seed, storage.EventSyncBatch{NextCursor: "old-sync-token", NextSyncAt: &nextSync}, true, now); err != nil {
				t.Fatal(err)
			}
			claim, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
			if err != nil || claim == nil {
				t.Fatalf("claim=%v %v", claim, err)
			}
			responseBody := fmt.Sprintf(`{"accessRole":%q,"items":[],"nextSyncToken":"next"}`, role)
			calls := 0
			original := http.DefaultTransport
			http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "www.googleapis.com" {
					return nil, fmt.Errorf("blocked synthetic host")
				}
				calls++
				if calls == 1 && r.URL.Query().Get("syncToken") != "old-sync-token" {
					t.Errorf("first page was not incremental")
				}
				if calls > 1 && r.URL.Query().Get("syncToken") != "" {
					t.Errorf("regrant did not refetch")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody)), Request: r}, nil
			})
			defer func() { http.DefaultTransport = original }()
			providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
			if err != nil {
				t.Fatal(err)
			}
			worker := eventsync.NewService(store, calendar.NewRegistry(providers), nil)
			worker.Now = func() time.Time { return now }
			if err = worker.RunOne(ctx, *claim); !errors.Is(err, storage.ErrAuthorityChanged) {
				t.Fatalf("role change=%v", err)
			}
			cals, err := store.ListCalendars(ctx, "account")
			if err != nil {
				t.Fatal(err)
			}
			if len(cals) != 1 || cals[0].CanRead || cals[0].DetailReceipt != "" {
				t.Fatal("source remained open")
			}
			events, _, err := store.ListCachedEvents(ctx, []string{id}, window.Start, window.End)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Fatal("old private title survived")
			}
			if role != "" {
				next, err := store.BeginAuthorization(ctx, "account", nil, now)
				if err != nil {
					t.Fatal(err)
				}
				entry.AccessRole = role
				if err = store.CompleteGoogleAuthorization(ctx, next, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
					t.Fatal(err)
				}
				responseBody = fmt.Sprintf(`{"accessRole":%q,"timeZone":"UTC","items":[{"id":"old","status":"confirmed","visibility":"private","start":{"dateTime":%q,"timeZone":"UTC"},"end":{"dateTime":%q,"timeZone":"UTC"}}],"nextSyncToken":"fresh"}`, role, now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
				claim, err = store.ClaimDueCalendarSync(ctx, "worker-2", now, now.Add(time.Minute))
				if err != nil || claim == nil {
					t.Fatalf("regrant claim=%v %v", claim, err)
				}
				providers, err = NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
				if err != nil {
					t.Fatal(err)
				}
				worker = eventsync.NewService(store, calendar.NewRegistry(providers), nil)
				worker.Now = func() time.Time { return now }
				if err = worker.RunOne(ctx, *claim); err != nil {
					t.Fatal(err)
				}
				events, _, err = store.ListCachedEvents(ctx, []string{id}, window.Start, window.End)
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 1 || events[0].Title != "" {
					t.Fatalf("redacted payload restored old title: %+v", events)
				}
			}
		})
	}
}

func TestVerifyGoogleActualFactoryPublishesEmptyAndDeniedDiscovery(t *testing.T) {
	cases := []struct {
		name, body string
		count      int
	}{
		{"empty", `{"items":[]}`, 0},
		{"all-denied", `{"items":[{"id":"a","accessRole":"freeBusyReader"},{"id":"b","accessRole":"none"}]}`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
			service := connections.New(store, cipher)
			if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "www.googleapis.com" {
					return nil, fmt.Errorf("blocked synthetic host")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			defer func() { http.DefaultTransport = original }()
			factory := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service)
			if err = service.VerifyGoogle(ctx, "account", factory.BuildGoogleVerification); err != nil {
				t.Fatal(err)
			}
			record, err := store.ConnectionByID(ctx, "account")
			if err != nil || record.Status != "connected" {
				t.Fatalf("status=%s err=%v", record.Status, err)
			}
			sources, err := store.ListCalendars(ctx, "account")
			if err != nil || len(sources) != tc.count {
				t.Fatalf("sources=%d err=%v", len(sources), err)
			}
			for _, c := range sources {
				if c.CanRead || c.DetailReceipt != "" {
					t.Fatal("denied source opened")
				}
			}
		})
	}
}

func TestVerifyGoogleActualFactoryDiscoveryStagesAllPages(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{10}, 32)))
			service := connections.New(store, cipher)
			if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
				t.Fatal(err)
			}
			calls := 0
			original := http.DefaultTransport
			http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "www.googleapis.com" {
					return nil, fmt.Errorf("blocked synthetic host")
				}
				calls++
				status := 200
				body := ""
				if calls == 1 {
					items := make([]string, 16)
					for i := range items {
						items[i] = fmt.Sprintf(`{"id":"cal-%d","accessRole":"reader"}`, i)
					}
					body = `{"items":[` + strings.Join(items, ",") + `],"nextPageToken":"next"}`
				} else if fail {
					status = 500
					body = `{}`
				} else {
					body = `{"items":[{"id":"cal-16","accessRole":"writerWithoutPrivateAccess"}]}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			defer func() { http.DefaultTransport = original }()
			factory := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service)
			err = service.VerifyGoogle(ctx, "account", factory.BuildGoogleVerification)
			if fail && err == nil || !fail && err != nil {
				t.Fatalf("verify error=%v", err)
			}
			cals, err := store.ListCalendars(ctx, "account")
			if err != nil {
				t.Fatal(err)
			}
			if fail && len(cals) != 0 {
				t.Fatal("partial discovery published")
			}
			if !fail && len(cals) != 17 {
				t.Fatalf("sources=%d", len(cals))
			}
			for _, c := range cals {
				if !c.CanRead || c.DetailReceipt == "" {
					t.Fatal("verified reader closed")
				}
			}
		})
	}
}

func TestFactoryRefreshThen401ClosesNewToken(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "expired", RefreshToken: "old-refresh", Expiry: now.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		body := ""
		status := 200
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			body = `{"access_token":"fresh","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`
		case "www.googleapis.com":
			status = 401
			body = `{}`
		default:
			return nil, fmt.Errorf("blocked synthetic host")
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); err == nil {
		t.Fatal("expected 401")
	}
	token, err := service.OAuthTokenStore("account", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "fresh" || token.RefreshToken != "rotated" {
		t.Fatal("rotated token not stored")
	}
	current, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if current.AuthorizationRevision == snap.AuthorizationRevision || current.Status != "error" || after[0].CanRead || before[0].DetailReceipt == after[0].DetailReceipt {
		t.Fatal("401 after refresh did not close authorization")
	}
}

func TestFactoryRefreshScopeDowngradeClosesGrant(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "expired", RefreshToken: "old-refresh", Expiry: now.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			body = `{"access_token":"fresh","refresh_token":"rotated","token_type":"Bearer","expires_in":3600,"scope":"https://www.googleapis.com/auth/calendar.calendarlist.readonly"}`
		case "www.googleapis.com":
			body = `{"accessRole":"owner","items":[]}`
		default:
			return nil, fmt.Errorf("blocked synthetic host")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); err == nil {
		t.Fatal("scope downgrade accepted")
	}
	token, err := service.OAuthTokenStore("account", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "expired" || token.RefreshToken != "old-refresh" {
		t.Fatal("downgraded token persisted")
	}
	current, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if current.AuthorizationRevision == snap.AuthorizationRevision || current.Status != "error" || after[0].CanRead || before[0].DetailReceipt == after[0].DetailReceipt {
		t.Fatal("scope change did not close authorization")
	}
}

func TestFactoryRefreshAllowsBroaderLegacyCalendarScope(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "expired", RefreshToken: "old-refresh", Expiry: now.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			body = `{"access_token":"fresh","refresh_token":"rotated","token_type":"Bearer","expires_in":3600,"scope":"https://www.googleapis.com/auth/calendar"}`
		case "www.googleapis.com":
			body = `{"accessRole":"owner","items":[]}`
		default:
			return nil, fmt.Errorf("blocked synthetic host")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("first refreshed request failed: %v", err)
	}
	token, err := service.OAuthTokenStore("account", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "fresh" || token.RefreshToken != "rotated" {
		t.Fatal("rotated token not stored")
	}
	current, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if current.AuthorizationRevision != snap.AuthorizationRevision || before[0].DetailReceipt != after[0].DetailReceipt || !after[0].CanRead {
		t.Fatal("ordinary refresh changed authorization")
	}
}

func TestRepairWriteOnlyDenialKeepsSourceReadable(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{12}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar", Timezone: "UTC"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	window := storage.SyncWindow{Start: now.Add(-24 * time.Hour), End: now.Add(48 * time.Hour)}
	if err = store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	seed, err := store.ClaimDueCalendarSync(ctx, "seed", now, now.Add(time.Hour))
	if err != nil || seed == nil {
		t.Fatalf("claim=%v", err)
	}
	next := now.Add(time.Minute)
	if err = store.ApplyEventSyncPage(ctx, *seed, storage.EventSyncBatch{Warnings: []storage.EventSyncWarning{{ObjectID: "broken", ETag: "etag", ErrorCode: "protocol"}}, NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ScheduleEventSyncObjectRepair(ctx, id, "broken", "etag", next); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimDueCalendarSync(ctx, "repair", next, next.Add(time.Hour))
	if err != nil || claim == nil {
		t.Fatalf("repair claim=%v", err)
	}
	ticket, err := store.CaptureWriterTicket(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	calls, patches := 0, 0
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" {
			return nil, fmt.Errorf("blocked synthetic host")
		}
		calls++
		status := 200
		var body string
		switch {
		case r.Method == "PATCH":
			patches++
			status = 403
			body = `{"error":{"errors":[{"reason":"forbiddenForNonOrganizer"}]}}`
		case strings.HasSuffix(r.URL.Path, "/calendars/cal"):
			body = `{"id":"cal","timeZone":"UTC"}`
		case strings.HasSuffix(r.URL.Path, "/events/broken"):
			date := now.Format("2006-01-02")
			body = fmt.Sprintf(`{"id":"broken","etag":"etag","status":"confirmed","start":{"date":%q},"end":{"date":%q}}`, date, date)
		default:
			body = `{"accessRole":"owner","items":[],"nextSyncToken":"cursor"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worker := eventsync.NewService(store, calendar.NewRegistry(providers), nil)
	worker.Now = func() time.Time { return next }
	if err = worker.RunOne(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || patches != 1 {
		t.Fatalf("calls=%d, patches=%d", calls, patches)
	}
	if err = store.ValidateAuthorityTickets(ctx, []storage.AuthorityTicket{ticket}); err != nil {
		t.Fatalf("read authority lost: %v", err)
	}
	diagnostics, err := store.ListActiveEventSyncQuarantineDiagnostics(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 || !diagnostics[0].Active {
		t.Fatal("object quarantine lost")
	}
}

func TestSameProviderOverlappingRefreshKeepsNewerGrant(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{13}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "expired", RefreshToken: "initial", Expiry: now.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var refreshes atomic.Int32
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		var status int
		var body string
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			n := refreshes.Add(1)
			status = 200
			body = fmt.Sprintf(`{"access_token":"access-%d","refresh_token":"refresh-%d","token_type":"Bearer","expires_in":1}`, n, n)
		case "www.googleapis.com":
			if r.Header.Get("Authorization") == "Bearer access-1" {
				close(firstStarted)
				<-release
				status = 401
				body = `{}`
			} else if r.Header.Get("Authorization") == "Bearer access-2" {
				status = 200
				body = `{"accessRole":"owner","items":[]}`
			} else {
				return nil, fmt.Errorf("unexpected synthetic token")
			}
		default:
			return nil, fmt.Errorf("blocked synthetic host")
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); done <- e }()
	<-firstStarted
	if _, err = providers[0].GetEvents(ctx, "cal", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("newer request=%v", err)
	}
	close(release)
	if err = <-done; err == nil {
		t.Fatal("expected old request failure")
	}
	record, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	cals, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "connected" || record.AuthorizationRevision != snap.AuthorizationRevision || !cals[0].CanRead {
		t.Fatal("old 401 closed newer token")
	}
}

func TestStaleRepair401DoesNotParkRefreshedGrant(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{14}, 32)))
	service := connections.New(store, cipher)
	if err = service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "old", RefreshToken: "old", Expiry: now.Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "account", "cal")
	entry := storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: "cal", Name: "Calendar"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	window := storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	if err = store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	seed, err := store.ClaimDueCalendarSync(ctx, "seed", now, now.Add(time.Hour))
	if err != nil || seed == nil {
		t.Fatal(err)
	}
	next := now.Add(time.Minute)
	if err = store.ApplyEventSyncPage(ctx, *seed, storage.EventSyncBatch{Warnings: []storage.EventSyncWarning{{ObjectID: "broken", ETag: "etag", ErrorCode: "protocol"}}, NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimDueCalendarSync(ctx, "repair", next, next.Add(time.Hour))
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" {
			return nil, fmt.Errorf("blocked synthetic host")
		}
		close(started)
		<-release
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	providers, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worker := eventsync.NewService(store, calendar.NewRegistry(providers), nil)
	worker.Now = func() time.Time { return next }
	done := make(chan error, 1)
	go func() { done <- worker.RunOne(ctx, *claim) }()
	<-started
	tokenStore := service.OAuthTokenStore("account", "google")
	old, err := tokenStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	old.AccessToken = "new"
	if err = tokenStore.Save(old); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-done; !errors.Is(err, storage.ErrAuthorityChanged) {
		t.Fatalf("stale repair result=%v", err)
	}
	record, err := store.ConnectionByID(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	cals, err := store.ListCalendars(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "connected" || record.AuthorizationRevision != snap.AuthorizationRevision || !cals[0].CanRead {
		t.Fatal("stale repair denied current grant")
	}
	ticket, err := store.CaptureWriterTicket(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateAuthorityTickets(ctx, []storage.AuthorityTicket{ticket}); err != nil {
		t.Fatalf("stale repair parked source: %v", err)
	}
	_, sources, err := store.ListCachedEvents(ctx, []string{id}, window.Start, window.End)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].ErrorCode == "auth" || sources[0].ErrorCode == "permission" {
		t.Fatalf("stale repair source status=%+v", sources)
	}
}

func TestOrdinaryGoogleListingRegistersMetadataWithoutOpeningDetailAuthority(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	service := connections.New(store, cipher)
	if err := service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.BeginAuthorization(ctx, "account", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	oldID := calendar.AccountCalendarID("google", "account", "existing")
	staged := []storage.DiscoveredDetailCalendar{{
		Calendar:   storage.Calendar{ID: oldID, ConnectionID: "account", ProviderCalendarID: "existing", Name: "Existing"},
		AccessRole: "owner",
	}}
	if err := store.CompleteGoogleAuthorization(ctx, snapshot, staged, now); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListCalendars(ctx, "account")
	if err != nil || len(before) != 1 || before[0].DetailReceipt == "" {
		t.Fatalf("staged calendar not authorized: %v", err)
	}

	original := http.DefaultTransport
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" || r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/calendarList") {
			return nil, errors.New("unexpected synthetic provider request")
		}
		body := `{"items":[{"id":"existing","summary":"Existing","accessRole":"owner"},{"id":"new","summary":"New","accessRole":"owner","timeZone":"Europe/Belgrade"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	factory := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service)
	providers, err := factory.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	registry := calendar.NewRegistry(providers, calendar.RegistryOptions{Selection: store})
	if list, err := registry.ListCalendars(ctx); err != nil || len(list) != 2 {
		t.Fatalf("ordinary listing: count=%d err=%v", len(list), err)
	}
	after, err := store.ListCalendars(ctx, "account")
	if err != nil || len(after) != 2 {
		t.Fatalf("metadata registration: count=%d err=%v", len(after), err)
	}
	if after[0].ID != oldID || after[0].DetailReceipt != before[0].DetailReceipt ||
		after[1].ProviderCalendarID != "new" || after[1].DetailReceipt != "" || after[1].Timezone != "Europe/Belgrade" {
		t.Fatal("ordinary discovery granted or changed detail authority")
	}
	if bootstrap, err := store.BeginOneLegacyGoogleAuthorization(ctx, now.Add(time.Minute)); err != nil || bootstrap != nil {
		t.Fatalf("ordinary discovery selected an established account for bootstrap: %v", err)
	}
}
