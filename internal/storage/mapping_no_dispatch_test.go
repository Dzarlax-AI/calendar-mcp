package storage

import (
	"errors"
	"testing"
	"time"
)

func TestRestoreMappingAfterNoDispatchRequiresLiveExactLease(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.CreateConnection(ctx, Connection{ID: "c", Provider: "google", AccountFingerprint: "synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "c", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateRule(ctx, Rule{ID: "r", SourceCalendarID: "source", TargetCalendarID: "target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueJob(ctx, Job{ID: "j", RuleID: "r", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	original := Mapping{ID: "m", RuleID: "r", SourceEventID: "event", TargetEventID: "copy", ContentHash: "old-hash", TargetETag: "old-etag", LastSeenAt: now, ReconciliationState: "current"}
	if err := s.UpsertMapping(ctx, original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE event_mappings SET target_etag=? WHERE id=?`, "old-etag", "m"); err != nil {
		t.Fatal(err)
	}
	mappings, err := s.ListMappings(ctx, "r")
	if err != nil || len(mappings) != 1 {
		t.Fatalf("mapping: %v", err)
	}
	original = mappings[0]
	pending := original
	pending.ReconciliationState = "pending_update"
	if err := s.UpdateMappingCAS(ctx, pending, original.Version); err != nil {
		t.Fatal(err)
	}
	pending.Version++
	previous := original
	previous.Version = pending.Version
	stale := *job
	stale.Attempt++
	if err := s.RestoreMappingAfterNoDispatchCAS(ctx, previous, pending.Version, stale); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("stale job restored: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET lease_until=? WHERE id=?`), now.Add(-time.Minute), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreMappingAfterNoDispatchCAS(ctx, previous, pending.Version, *job); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("expired job restored: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET lease_until=? WHERE id=?`), now.Add(time.Hour), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreMappingAfterNoDispatchCAS(ctx, previous, pending.Version, *job); err != nil {
		var details string
		_ = s.db.QueryRowContext(ctx, `SELECT printf('%s | %s | %s | %s | %s',claimed_at,lease_until,datetime('now'),julianday(claimed_at),julianday(lease_until)) FROM sync_jobs WHERE id='j'`).Scan(&details)
		t.Fatalf("%v %s", err, details)
	}
	got, err := s.ListMappings(ctx, "r")
	if err != nil || len(got) != 1 {
		t.Fatalf("read: %v", err)
	}
	if got[0].ReconciliationState != "current" || got[0].ContentHash != "old-hash" || got[0].TargetETag != "old-etag" || got[0].Incarnation != original.Incarnation || got[0].Version != pending.Version+1 {
		t.Fatalf("restoration: %+v", got[0])
	}
	if err := s.RestoreMappingAfterNoDispatchCAS(ctx, previous, pending.Version, *job); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("stale CAS restored: %v", err)
	}
}
