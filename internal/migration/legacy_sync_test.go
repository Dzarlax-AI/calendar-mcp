package migration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/storage"
)

func TestLoadBuildsPausedLegacyPlan(t *testing.T) {
	stateFile := writeState(t, `{"last_sync":"2026-08-20T10:00:00Z","mappings":{"source-2":{"google_id":"target-2","hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"source-1":{"google_id":"target-1","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	plan, err := Load(stateFile, "microsoft:source", "google:target", now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Rule.State != "paused" || plan.Rule.LookbackDays != 0 || plan.Rule.LookaheadDays != 14 {
		t.Fatalf("rule = %#v", plan.Rule)
	}
	if len(plan.Mappings) != 2 || plan.Mappings[0].SourceEventID != "source-1" {
		t.Fatalf("mappings = %#v", plan.Mappings)
	}
	for _, mapping := range plan.Mappings {
		if mapping.RuleID != plan.Rule.ID || mapping.ReconciliationState != "legacy" {
			t.Fatalf("mapping = %#v", mapping)
		}
	}
}

func TestLoadRejectsDuplicateAndMissingTargets(t *testing.T) {
	tests := []struct {
		name, state, want string
	}{
		{
			name:  "duplicate source key",
			state: `{"mappings":{"source":{"google_id":"one","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"source":{"google_id":"two","hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}`,
			want:  "duplicate JSON key",
		},
		{
			name:  "missing target",
			state: `{"mappings":{"source":{"google_id":"","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`,
			want:  "no target event id",
		},
		{
			name:  "shared target",
			state: `{"mappings":{"one":{"google_id":"target","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"two":{"google_id":"target","hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}`,
			want:  "mapped from both",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeState(t, tt.state), "microsoft:source", "google:target", time.Now())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestImportWritesRuleAndMappingsAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for _, connection := range []storage.Connection{
		{ID: "microsoft-connection", Provider: "microsoft", DisplayName: "Microsoft", Status: "connected", EncryptedCredentials: []byte("encrypted"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now},
		{ID: "google-connection", Provider: "google", DisplayName: "Google", Status: "connected", EncryptedCredentials: []byte("encrypted"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.CreateConnection(ctx, connection); err != nil {
			t.Fatal(err)
		}
	}
	for _, calendar := range []storage.Calendar{
		{ID: "microsoft:source", ConnectionID: "microsoft-connection", ProviderCalendarID: "source", Name: "Source", CanRead: true, DiscoveredAt: now},
		{ID: "google:target", ConnectionID: "google-connection", ProviderCalendarID: "target", Name: "Target", CanRead: true, CanWrite: true, DiscoveredAt: now},
	} {
		if err := store.UpsertCalendar(ctx, calendar); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Load(writeState(t, `{"mappings":{"source-event":{"google_id":"target-event","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`), "microsoft:source", "google:target", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := Import(ctx, store, plan); err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].State != "paused" {
		t.Fatalf("rules = %#v, err = %v", rules, err)
	}
	mappings, err := store.ListMappings(ctx, plan.Rule.ID)
	if err != nil || len(mappings) != 1 || mappings[0].ReconciliationState != "legacy" {
		t.Fatalf("mappings = %#v, err = %v", mappings, err)
	}
	if err := Import(ctx, store, plan); err == nil {
		t.Fatal("second import unexpectedly succeeded")
	}
	rules, _ = store.ListRules(ctx)
	mappings, _ = store.ListMappings(ctx, plan.Rule.ID)
	if len(rules) != 1 || len(mappings) != 1 {
		t.Fatalf("repeat import was not atomic: rules=%d mappings=%d", len(rules), len(mappings))
	}
}

func writeState(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sync_state.json")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
