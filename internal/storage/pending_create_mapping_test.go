package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLitePendingCreateMappingRace(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "pending.db")
	first, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err = first.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runPendingCreateMappingRace(t, first, second, fmt.Sprintf("pending-%d", time.Now().UnixNano()))
}

func runPendingCreateMappingRace(t *testing.T, first, second *Store, id string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	if err := first.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM event_mappings WHERE rule_id=?`), id)
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, cal := range []string{id + "-source", id + "-target"} {
		if err := first.UpsertCalendar(ctx, Calendar{ID: cal, ConnectionID: id, ProviderCalendarID: cal, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.CreateRule(ctx, Rule{ID: id, SourceCalendarID: id + "-source", TargetCalendarID: id + "-target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	makeIntent := func(suffix, event string) Mapping {
		mid := id + suffix
		return Mapping{ID: mid, RuleID: id, ObjectKind: "single", SourceEventID: event, TargetEventID: "pending:" + mid, LastSeenAt: now, ReconciliationState: "pending_create"}
	}
	results := make(chan struct {
		ok  string
		err error
	}, 2)
	for i, s := range []*Store{first, second} {
		m := makeIntent(fmt.Sprintf("-race-%d", i), "same-source")
		go func() {
			ok, err := s.InsertPendingCreateMapping(ctx, m)
			results <- struct {
				ok  string
				err error
			}{ok, err}
		}()
	}
	wins := 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.ok != "" {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent winners=%d", wins)
	}
	current := Mapping{ID: id + "-current", RuleID: id, ObjectKind: "single", SourceEventID: "already-mapped", TargetEventID: "existing-event", LastSeenAt: now, ReconciliationState: "current"}
	if err := first.UpsertMapping(ctx, current); err != nil {
		t.Fatal(err)
	}
	if ok, err := second.InsertPendingCreateMapping(ctx, makeIntent("-collision", "already-mapped")); err != nil || ok != "" {
		t.Fatalf("existing mapping replaced: %v %v", ok, err)
	}
	items, err := first.ListMappings(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("unexpected mapping count %d", len(items))
	}
	for _, m := range items {
		if m.SourceEventID == "already-mapped" && (m.TargetEventID != current.TargetEventID || m.ReconciliationState != "current") {
			t.Fatalf("existing mapping changed: %+v", m)
		}
	}
}
