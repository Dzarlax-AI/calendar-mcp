package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
	"golang.org/x/oauth2"
)

func TestGoogleConfirmedEvidenceClosesAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		list       bool
	}{
		{"list role", `{"items":[{"id":"cal","accessRole":"freeBusyReader"}]}`, 200, true},
		{"event role", `{"accessRole":"freeBusyReader","items":[]}`, 200, false},
		{"denial", `{"error":{}}`, 403, false},
	} {
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
			original := http.DefaultTransport
			defer func() { http.DefaultTransport = original }()
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			calls := 0
			http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				cancel()
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			ps, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.list {
				_, err = ps[0].ListCalendars(requestCtx)
			} else {
				_, err = ps[0].GetEvents(requestCtx, "cal", now, now.Add(time.Hour))
			}
			if err == nil {
				t.Fatal("expected cancelled or closed request")
			}
			rows, err := store.ListCalendars(ctx, "account")
			if err != nil || len(rows) != 1 || rows[0].CanRead || calls != 1 {
				t.Fatalf("rows=%+v err=%v calls=%d", rows, err, calls)
			}
		})
	}
}

func TestGoogleListClosesEveryDowngradeOnOnePage(t *testing.T) {
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
	var entries []storage.DiscoveredDetailCalendar
	for _, raw := range []string{"a", "b"} {
		entries = append(entries, storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: calendar.AccountCalendarID("google", "account", raw), ConnectionID: "account", ProviderCalendarID: raw, Name: raw}, AccessRole: "owner"})
	}
	if err = store.CompleteGoogleAuthorization(ctx, snap, entries, now); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	calls := 0
	http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[{"id":"a","accessRole":"reader"},{"id":"b","accessRole":"freeBusyReader"}]}`)), Request: r}, nil
	})
	ps, err := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ps[0].ListCalendars(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	rows, err := store.ListCalendars(ctx, "account")
	if err != nil || len(rows) != 2 || rows[0].CanRead || rows[1].CanRead || calls != 1 {
		t.Fatalf("rows=%+v err=%v calls=%d", rows, err, calls)
	}
}

func TestStrictGooglePublicationLeavesOrdinaryReadAuthorityIntact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"role changed", http.StatusOK, `{"accessRole":"reader","items":[]}`},
		{"confirmed denial", http.StatusForbidden, `{"error":{"errors":[{"reason":"forbidden"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().UTC().Truncate(time.Millisecond)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db"))
			must(err)
			defer store.Close()
			must(store.Migrate(ctx))
			cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
			must(err)
			service := connections.New(store, cipher)
			must(service.CreateOAuth(ctx, "account", "google", "Google", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic", Expiry: now.Add(time.Hour)}, nil))
			snapshot, err := store.BeginAuthorization(ctx, "account", nil, now)
			must(err)
			const raw = "target"
			id := calendar.AccountCalendarID("google", "account", raw)
			must(store.CompleteGoogleAuthorization(ctx, snapshot, []storage.DiscoveredDetailCalendar{{
				Calendar:   storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: raw, Name: "Fixture"},
				AccessRole: "owner",
			}}, now))
			current, err := store.ListCalendars(ctx, "account")
			must(err)
			receipt := current[0].DetailReceipt
			lease, err := store.ClaimGatewayAuthorityCheck(ctx, "account", now, time.Minute)
			must(err)
			must(store.PublishGatewayAuthority(ctx, lease, []storage.GatewayCalendarProof{{
				Metadata:     calendar.Calendar{ID: id, Provider: "google"},
				Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "google"},
				Class:        storage.GatewayPrivateAccess, GoogleRole: "owner", DetailReceipt: receipt,
			}}, now))
			grant, err := store.GatewayCalendarGrant(ctx, id, now)
			must(err)
			original := http.DefaultTransport
			defer func() { http.DefaultTransport = original }()
			calls := 0
			http.DefaultTransport = syntheticTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet {
					t.Fatalf("publication dispatched %s", r.Method)
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			factory := NewFactory(&config.Config{GoogleClientID: "id", GoogleClientSecret: "secret"}, store, service)
			p, providerID, err := factory.BuildGatewayGooglePublication(ctx, storage.GatewayPublicationTicket{Grant: grant})
			must(err)
			_, err = p.(interface {
				ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error)
			}).ListEventsV2(ctx, calendar.ListEventsRequestV2{CalendarID: providerID,
				Start: now.Add(-time.Hour), End: now.Add(time.Hour), View: calendar.RecurrenceExpanded})
			if err == nil || calls != 1 {
				t.Fatalf("strict probe accepted response or did not call provider: err=%v calls=%d", err, calls)
			}
			if _, err := store.GatewayCalendarGrant(ctx, id, now); !errors.Is(err, storage.ErrAuthorityChanged) {
				t.Fatalf("strict grant was not closed: %v", err)
			}
			after, err := store.ListCalendars(ctx, "account")
			must(err)
			if len(after) != 1 || !after[0].CanRead || after[0].DetailReceipt != receipt {
				t.Fatal("strict probe changed the ordinary Google receipt")
			}
		})
	}
}
