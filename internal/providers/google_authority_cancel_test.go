package providers

import (
	"bytes"
	"context"
	"encoding/base64"
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
