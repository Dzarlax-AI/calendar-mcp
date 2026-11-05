package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestMappingIncarnationMigrationPreservesLegacyRows(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 25; version++ {
		script, err := migrations.ReadFile(fmt.Sprintf("migrations/sqlite/%03d_%s.sql", version, migrationName(version)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, Connection{ID: "conn", Provider: "google", AccountFingerprint: "conn", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err = s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "conn", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO sync_rules(id,source_calendar_id,target_calendar_id,state,interval_seconds,lookback_days,lookahead_days,recurrence_mode,copy_attendees,notification_policy,consecutive_failures,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "rule", "source", "target", "paused", 600, 0, 14, "preserve", false, "none", 0, now, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO event_mappings(id,rule_id,object_kind,source_event_id,target_event_id,content_hash,last_seen_at,reconciliation_state) VALUES(?,?,?,?,?,?,?,?)`, id, "rule", "event", id, "target-"+id, "hash", now, "legacy"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListMappings(ctx, "rule")
	if err != nil || len(items) != 2 {
		t.Fatalf("mappings: %+v %v", items, err)
	}
	for _, m := range items {
		if m.Version != 1 || m.Incarnation != "legacy:"+m.ID {
			t.Fatalf("wrong migrated mapping: %+v", m)
		}
		m.ReconciliationState = "pending_update"
		if err = s.UpdateMappingCAS(ctx, m, 1); err != nil {
			t.Fatal(err)
		}
	}
}
