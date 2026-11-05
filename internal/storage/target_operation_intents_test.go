package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSQLiteTargetIntentContract(t *testing.T) {
	s := newSQLiteStore(t)
	ensureTargetIntentTestSchema(t, s)
	runTargetIntentContract(t, s)
}

func ensureTargetIntentTestSchema(t *testing.T, s *Store) {
	t.Helper()
	var exists bool
	q := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='target_operation_intents')`
	if s.dialect == DialectPostgres {
		q = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='target_operation_intents')`
	}
	if err := s.db.QueryRowContext(t.Context(), q).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		script, err := migrations.ReadFile(fmt.Sprintf("migrations/%s/015_target_operation_intents.sql", s.dialect))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
			t.Fatal(err)
		}
	}
}
func runTargetIntentContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	id := fmt.Sprintf("intent-%d", now.UnixNano())
	source, target := id+"-source", id+"-target"
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM target_operation_intents WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, cal := range []string{source, target} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: cal, ConnectionID: id, ProviderCalendarID: cal, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	base := int64(0)
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	snapshot := ProjectionSnapshotInput{CalendarID: source, StageID: "test_stage_identifier_123", Timezone: "UTC", BaseRevision: &base, GuardEpoch: 0, WindowStart: from, WindowEnd: to, CompletedAt: now, Cursor: "synthetic", Payload: []byte(`[]`), Complete: true}
	if err := s.StageProjectionSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, source, snapshot.StageID); err != nil {
		t.Fatal(err)
	}
	rule := Rule{ID: id, SourceCalendarID: source, TargetCalendarID: target, State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueJob(ctx, Job{ID: id, RuleID: id, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("claim: %v", err)
	}
	if s.dialect == DialectPostgres {
		// StartRun inserts FKs for both job and rule. The intent's non-key
		// row locks must not block those KEY SHARE checks.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		var locked string
		if err := tx.QueryRowContext(ctx, s.query(`SELECT id FROM sync_rules WHERE id=? FOR NO KEY UPDATE`), id).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRowContext(ctx, s.query(`SELECT id FROM sync_jobs WHERE id=? FOR NO KEY UPDATE`), id).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		startCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = s.StartRun(startCtx, Run{ID: id, JobID: id, RuleID: id, Trigger: "manual", Outcome: "running", StartedAt: now})
		cancel()
		if err != nil {
			t.Fatalf("StartRun blocked by non-key intent locks: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	in := TargetIntentRequest{OperationID: "operation_identifier_0001", RuleID: id, JobID: id, WorkerID: "worker", JobAttempt: job.Attempt, RuleVersion: 1, SourceCalendarID: source, TargetCalendarID: target, SourceRevision: 1, WindowStart: from, WindowEnd: to, IdentityHash: stagingDigest("identity"), PayloadHash: stagingDigest("payload"), Kind: "create", OwnershipMarker: "synthetic-marker", Now: now, LeaseDuration: time.Minute, SnapshotMaxAge: time.Minute}
	if err := s.PrepareTargetIntent(ctx, in); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("unverified revision: %v", err)
	}
	seedVerifiedProjectionSnapshot(t, s, source, 1)
	bad := in
	bad.RuleVersion = 99
	if err := s.PrepareTargetIntent(ctx, bad); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("stale rule: %v", err)
	}
	bad = in
	bad.JobAttempt++
	if err := s.PrepareTargetIntent(ctx, bad); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("stale job: %v", err)
	}
	bad = in
	bad.WindowEnd = to.Add(time.Second)
	if err := s.PrepareTargetIntent(ctx, bad); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("coverage: %v", err)
	}
	bad = in
	bad.Kind = "delete"
	if err := s.PrepareTargetIntent(ctx, bad); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("unguarded delete: %v", err)
	}
	// A caller timestamp that was valid before waiting for locks must not
	// authorize a claim that has expired by the time the transaction holds them.
	oldClaim := now.Add(-2 * time.Minute)
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET claimed_at=? WHERE id=?`), oldClaim, id); err != nil {
		t.Fatal(err)
	}
	seedVerifiedProjectionSnapshotAt(t, s, source, 1, &oldClaim)
	bad = in
	bad.Now = oldClaim.Add(10 * time.Second)
	bad.SnapshotMaxAge = time.Hour
	if err := s.PrepareTargetIntent(ctx, bad); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("aged claim: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET claimed_at=? WHERE id=?`), now, id); err != nil {
		t.Fatal(err)
	}
	seedVerifiedProjectionSnapshotAt(t, s, source, 1, &now)
	bad = in
	bad.Now = now.Add(2 * time.Minute)
	bad.LeaseDuration = 15 * time.Minute
	if err := s.PrepareTargetIntent(ctx, bad); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("stale snapshot: %v", err)
	}
	if err := s.SetCalendarEnabled(ctx, target, false, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareTargetIntent(ctx, in); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("disabled target: %v", err)
	}
	if err := s.SetCalendarEnabled(ctx, target, true, now); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for n := 0; n < 2; n++ {
		v := in
		v.OperationID = fmt.Sprintf("operation_identifier_%04d", n)
		go func() { errs <- s.PrepareTargetIntent(ctx, v) }()
	}
	success, blocked := 0, 0
	for n := 0; n < 2; n++ {
		err := <-errs
		if err == nil {
			success++
		} else if errors.Is(err, ErrTargetIntentBlocked) {
			blocked++
		} else {
			t.Fatalf("race: %v", err)
		}
	}
	if success != 1 || blocked != 1 {
		t.Fatalf("race: %d success %d blocked", success, blocked)
	}
	var chosen string
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT operation_id FROM target_operation_intents WHERE rule_id=?`), id).Scan(&chosen); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTargetIntentAmbiguous(ctx, chosen, now); err != nil {
		t.Fatal(err)
	}
	out, err := s.GetTargetIntentStatus(ctx, chosen)
	if err != nil || out.Status != "ambiguous" {
		t.Fatalf("status: %v", err)
	}
	in.OperationID = chosen
	in.IdentityHash = stagingDigest("another-identity")
	if err := s.PrepareTargetIntent(ctx, in); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("operation ID reused: %v", err)
	}
	in.IdentityHash = stagingDigest("identity")
	in.OperationID = "operation_identifier_9999"
	if err := s.PrepareTargetIntent(ctx, in); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("ambiguous replay: %v", err)
	}
}
