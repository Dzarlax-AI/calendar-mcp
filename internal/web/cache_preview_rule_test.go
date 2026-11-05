package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/storage"
)

func TestCreateCachePreviewRulePersistsModeButCannotEnable(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, storage.Connection{ID: "conn", Provider: "google", AccountFingerprint: "conn", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err = s.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: "conn", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	values := url.Values{"sync_mode": {"cached_instances"}, "source_calendar_id": {"source"}, "target_calendar_id": {"target"}, "interval_seconds": {"600"}, "lookback_days": {"0"}, "lookahead_days": {"14"}}
	req := httptest.NewRequest(http.MethodPost, "/rules", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	(&Server{store: s}).createRule(res, req)
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/rules?status=rule_created" {
		t.Fatalf("create %d %s", res.Code, res.Header().Get("Location"))
	}
	rules, err := s.ListRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].SyncMode != "cached_instances" || rules[0].State != "paused" {
		t.Fatalf("rules: %+v %v", rules, err)
	}
	if err = s.SetRuleState(ctx, rules[0].ID, "enabled", nil, now); err == nil {
		t.Fatal("enabled preview-only rule")
	}
}
