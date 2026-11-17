package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

func TestRuntimeCachedMCPDependencyRequiresAllPrerequisites(t *testing.T) {
	store, err := storage.Open(t.Context(), "sqlite://"+filepath.Join(t.TempDir(), "runtime-cached.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ready", "no_v2", "no_model", "no_store", "no_cipher", "no_primary_key"} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{EnableV2: true, EventReadModelEnabled: true, APIKey: "synthetic-primary", EventCacheLookbackDays: 1, EventCacheLookaheadDays: 2}
			db, key := store, cipher
			switch name {
			case "no_v2":
				cfg.EnableV2 = false
			case "no_model":
				cfg.EventReadModelEnabled = false
			case "no_store":
				db = nil
			case "no_cipher":
				key = nil
			case "no_primary_key":
				cfg.APIKey = ""
			}
			opts, err := runtimeMCPOptions(cfg, db, key, func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) })
			if err != nil {
				t.Fatal(err)
			}
			if (opts.CachedEvents != nil) != (name == "ready") {
				t.Fatalf("cached dependency presence=%v", opts.CachedEvents != nil)
			}
			if name == "ready" && !opts.CachedEvents.Ready() {
				t.Fatal("configured dependency not ready")
			}
		})
	}
}

func TestRuntimeCachedReaderUsesLiveClock(t *testing.T) {
	store, err := storage.Open(t.Context(), "sqlite://"+filepath.Join(t.TempDir(), "rolling.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 23, 59, 0, 0, time.UTC)
	calls := 0
	cfg := &config.Config{EnableV2: true, EventReadModelEnabled: true, APIKey: "synthetic-primary", EventCacheLookbackDays: 1, EventCacheLookaheadDays: 2}
	opts, err := runtimeMCPOptions(cfg, store, cipher, func() time.Time { calls++; return now })
	if err != nil || opts.CachedEvents == nil {
		t.Fatalf("reader: %v", err)
	}
	var ctx context.Context
	handler := auth.Middleware(auth.Options{APIKey: cfg.APIKey})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	calls = 0
	now = now.Add(2 * time.Minute)
	window := eventReadModelWindow(cfg, now)
	_, _, _ = opts.CachedEvents.Read(ctx, cachedpages.Request{CalendarIDs: []string{"google:conn:primary"}, Start: window.Start, End: window.Start.Add(time.Hour)}, "")
	if calls == 0 {
		t.Fatal("runtime reader did not consult the live clock")
	}
}
