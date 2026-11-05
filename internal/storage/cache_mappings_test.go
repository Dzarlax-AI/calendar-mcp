package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func ensureCacheMappingTestSchema(t *testing.T, s *Store) {
	t.Helper()
	query := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='cache_event_mappings')`
	if s.dialect == DialectPostgres {
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='cache_event_mappings')`
	}
	var exists bool
	if err := s.db.QueryRowContext(t.Context(), query).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		return
	}
	script, err := migrations.ReadFile(fmt.Sprintf("migrations/%s/021_cache_mappings.sql", s.dialect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteCacheMappingAssertion(t *testing.T) {
	s := newSQLiteStore(t)
	ensureCacheMappingTestSchema(t, s)
	runCacheMappingAssertionContract(t, s)
}
func runCacheMappingAssertionContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fmt.Sprintf("cache-mapping-%d", now.UnixNano())
	source, target := id+"-source", id+"-target"
	otherRule := id + "-other"
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM cache_event_mappings WHERE rule_id=? OR rule_id=?`), id, otherRule)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM sync_rules WHERE id=? OR id=?`), id, otherRule)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, cal := range []string{source, target} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: cal, ConnectionID: id, ProviderCalendarID: cal, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	base := int64(0)
	snap := ProjectionSnapshotInput{CalendarID: source, StageID: "mapping-stage-0123456789", Timezone: "UTC", BaseRevision: &base, WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(time.Hour), CompletedAt: now, Cursor: "synthetic-cursor", Payload: []byte(`[]`), Complete: true}
	if err := s.StageProjectionSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, source, snap.StageID); err != nil {
		t.Fatal(err)
	}
	for _, rid := range []string{id, otherRule} {
		if err := s.CreateRule(ctx, Rule{ID: rid, SourceCalendarID: source, TargetCalendarID: target, State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	m := CacheMapping{RuleID: id, IdentityHash: stagingDigest("identity"), SourceCalendarID: source, SourceRevision: 1, TargetCalendarID: target, TargetEventID: "abcde12345", OwnershipMarker: "synthetic-marker", TargetETag: "synthetic-etag", ContentHash: stagingDigest("content"), RuleVersion: 1, UpdatedAt: now}
	if err := s.PutCacheMappingAssertion(ctx, m, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("unverified revision: %v", err)
	}
	seedVerifiedProjectionSnapshot(t, s, source, 1)
	bad := m
	bad.SourceRevision = 2
	if err := s.PutCacheMappingAssertion(ctx, bad, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("missing revision: %v", err)
	}
	bad = m
	bad.RuleVersion = 2
	if err := s.PutCacheMappingAssertion(ctx, bad, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("wrong rule version: %v", err)
	}
	bad = m
	bad.IdentityHash = "bad"
	if err := s.PutCacheMappingAssertion(ctx, bad, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("bad hash: %v", err)
	}
	bad = m
	bad.TargetEventID = "google:" + m.TargetEventID
	if err := s.PutCacheMappingAssertion(ctx, bad, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("prefixed alias: %v", err)
	}
	bad = m
	bad.TargetEventID = "microsoft:event"
	if err := s.PutCacheMappingAssertion(ctx, bad, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("unsupported event ID: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE connections SET provider=? WHERE id=?`), "microsoft", id); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCacheMappingAssertion(ctx, m, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("unsupported provider: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE connections SET provider=? WHERE id=?`), "google", id); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCacheMappingAssertion(ctx, m, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCacheMappingAssertion(ctx, m, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("duplicate identity: %v", err)
	}
	other := m
	other.RuleID = otherRule
	other.IdentityHash = stagingDigest("other-identity")
	if err := s.PutCacheMappingAssertion(ctx, other, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("duplicate target ownership: %v", err)
	}
	other.TargetEventID = "abcde98765"
	if err := s.PutCacheMappingAssertion(ctx, other, 0); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("duplicate ownership marker: %v", err)
	}

	got, err := s.GetCacheMapping(ctx, id, m.IdentityHash)
	if err != nil || got.MappingVersion != 1 || got.TargetEventID != m.TargetEventID {
		t.Fatalf("initial read: %v", err)
	}
	bad = got
	bad.TargetEventID = "bcdef12345"
	if err := s.PutCacheMappingAssertion(ctx, bad, 1); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("changed target: %v", err)
	}
	bad = got
	bad.OwnershipMarker = "different"
	if err := s.PutCacheMappingAssertion(ctx, bad, 1); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("changed owner: %v", err)
	}
	errorsCh := make(chan error, 2)
	for n := 0; n < 2; n++ {
		candidate := got
		candidate.ContentHash = stagingDigest(fmt.Sprintf("next-%d", n))
		go func() { errorsCh <- s.PutCacheMappingAssertion(ctx, candidate, 1) }()
	}
	success, blocked := 0, 0
	for n := 0; n < 2; n++ {
		err := <-errorsCh
		if err == nil {
			success++
		} else if errors.Is(err, ErrCacheMappingConflict) {
			blocked++
		} else {
			t.Fatalf("race: %v", err)
		}
	}
	if success != 1 || blocked != 1 {
		t.Fatalf("CAS successes=%d conflicts=%d", success, blocked)
	}
	got, err = s.GetCacheMapping(ctx, id, m.IdentityHash)
	if err != nil || got.MappingVersion != 2 {
		t.Fatalf("version read: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_snapshots WHERE calendar_id=? AND revision=?`), source, 1); err == nil {
		t.Fatal("deleted referenced source revision")
	}
	if err := s.SetRuleState(ctx, id, "enabled", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCacheMappingAssertion(ctx, got, 2); !errors.Is(err, ErrCacheMappingConflict) {
		t.Fatalf("stale rule allowed: %v", err)
	}
}
