//go:build integration

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresMappingCASConcurrent(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	s, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	now := time.Now().UTC()
	id := fmt.Sprintf("mapping-cas-%d", now.UnixNano())
	if err = s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM event_mappings WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, c := range []string{id + "-source", id + "-target"} {
		if err = s.UpsertCalendar(ctx, Calendar{ID: c, ConnectionID: id, ProviderCalendarID: c, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.CreateRule(ctx, Rule{ID: id, SourceCalendarID: id + "-source", TargetCalendarID: id + "-target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	m := Mapping{ID: id + "-mapping", RuleID: id, SourceEventID: "event", TargetEventID: "target", ReconciliationState: "current", LastSeenAt: now}
	if err = s.UpsertMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListMappings(ctx, id)
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v", err)
	}
	m = items[0]
	results := make(chan error, 2)
	for _, state := range []string{"pending_update", "pending_delete"} {
		v := m
		v.ReconciliationState = state
		go func() { results <- s.UpdateMappingCAS(ctx, v, 1) }()
	}
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ErrMappingConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	if err = s.DeleteMapping(ctx, m.ID); err == nil {
		t.Fatal("legacy deletion removed pending")
	}
	items, err = s.ListMappings(ctx, id)
	if err != nil || len(items) != 1 {
		t.Fatalf("list pending: %v", err)
	}
	if err = s.DeleteMappingCAS(ctx, items[0].ID, items[0].Incarnation, items[0].Version); err != nil {
		t.Fatal(err)
	}
	pending := m
	pending.Version = 0
	pending.Incarnation = ""
	pending.ReconciliationState = "pending_create"
	pending.TargetEventID = "pending:" + pending.ID
	incarnation, err := s.InsertPendingCreateMapping(ctx, pending)
	if err != nil || incarnation == "" || incarnation == m.Incarnation {
		t.Fatalf("recreate: %v", err)
	}
	if err := s.UpdateMappingCAS(ctx, m, 1); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("ABA update accepted: %v", err)
	}
	if err := s.DeleteMappingCAS(ctx, m.ID, m.Incarnation, 1); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("ABA delete accepted: %v", err)
	}
}
