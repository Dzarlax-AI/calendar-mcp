package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func ensureTargetGuardLinks(t *testing.T, s *Store) {
	t.Helper()
	q := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='target_operation_guard_links')`
	if s.dialect == DialectPostgres {
		q = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='target_operation_guard_links')`
	}
	var found bool
	if err := s.db.QueryRowContext(t.Context(), q).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found {
		return
	}
	script, err := migrations.ReadFile(fmt.Sprintf("migrations/%s/023_target_guard_links.sql", s.dialect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteExperimentalTargetGuardAtomic(t *testing.T) {
	s := newSQLiteStore(t)
	ensureTargetGuardLinks(t, s)
	runExperimentalTargetGuardAtomic(t, s)
}
func runExperimentalTargetGuardAtomic(t *testing.T, s *Store) {
	t.Helper()
	ensureTargetReceiptTestSchema(t, s)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fmt.Sprintf("experimental-%d", now.UnixNano())
	source, target := id+"-source", id+"-target"
	otherTarget := id + "-other-target"
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM target_operation_receipts WHERE operation_id LIKE ?`), id+"%")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM exclusive_dispatch_guard_members WHERE operation_id LIKE ?`), id+"%")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM exclusive_dispatch_reservations WHERE operation_id LIKE ?`), id+"%")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM target_operation_guard_links WHERE calendar_id=?`), target)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_operation_members WHERE calendar_id=?`), target)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_intents WHERE calendar_id=?`), target)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_operations WHERE operation_id LIKE ?`), id+"%")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM target_operation_intents WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_snapshot_pins WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, cal := range []string{source, target, otherTarget} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: cal, ConnectionID: id, ProviderCalendarID: cal, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET cursor=?,status=? WHERE calendar_id=?`), "old-cursor", "ready", target); err != nil {
		t.Fatal(err)
	}
	base := int64(0)
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	stage := ProjectionSnapshotInput{CalendarID: source, StageID: id + "-source-stage", Timezone: "UTC", BaseRevision: &base, WindowStart: start, WindowEnd: end, CompletedAt: now, Cursor: "synthetic", Payload: []byte(`[]`), Complete: true}
	if err := s.StageProjectionSnapshot(ctx, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, source, stage.StageID); err != nil {
		t.Fatal(err)
	}
	seedVerifiedProjectionSnapshot(t, s, source, 1)
	if err := s.CreateRule(ctx, Rule{ID: id, SourceCalendarID: source, TargetCalendarID: target, State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueJob(ctx, Job{ID: id, RuleID: id, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("claim: %v", err)
	}
	pin := SnapshotPinRequest{PinID: id + "-pin-identifier", RuleID: id, JobID: id, WorkerID: "worker", CalendarID: source, RuleVersion: 1, Revision: 1, JobAttempt: job.Attempt, Now: now, ExpiresAt: now.Add(30 * time.Second), LeaseDuration: time.Minute}
	if err = s.AcquireSnapshotPin(ctx, pin); err != nil {
		t.Fatal(err)
	}
	in := TargetIntentRequest{OperationID: id + "-operation-0", RuleID: id, JobID: id, WorkerID: "worker", JobAttempt: job.Attempt, RuleVersion: 1, SourceCalendarID: source, TargetCalendarID: target, SourceRevision: 1, WindowStart: start, WindowEnd: end, IdentityHash: stagingDigest("identity"), PayloadHash: stagingDigest("payload"), Kind: "create", OwnershipMarker: "synthetic-marker", Now: now, LeaseDuration: time.Minute, SnapshotMaxAge: time.Minute}
	// The rule is A -> B; a request targeting C must not leave any part of
	// a ledger, operation, target guard, link, or attendee state.
	sourceBefore, err := s.InspectProjectionMutations(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	wrongTarget := in
	wrongTarget.TargetCalendarID = otherTarget
	if err := s.experimentalBeginTargetGuard(ctx, wrongTarget, pin); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("wrong target accepted: %v", err)
	}
	if _, err := s.GetProjectionMutationMany(ctx, wrongTarget.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong target left ledger: %v", err)
	}
	if _, err := s.GetTargetIntentStatus(ctx, wrongTarget.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong target left intent: %v", err)
	}
	if _, err := s.InspectProjectionMutations(ctx, otherTarget); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong target left guard: %v", err)
	}
	sourceAfter, err := s.InspectProjectionMutations(ctx, source)
	if err != nil || sourceAfter != sourceBefore {
		t.Fatalf("wrong target changed source guard: %v", err)
	}
	if _, err := s.InspectProjectionMutations(ctx, target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong target changed rule destination guard: %v", err)
	}
	var links int
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM target_operation_guard_links WHERE operation_id=?`), wrongTarget.OperationID).Scan(&links); err != nil || links != 0 {
		t.Fatalf("wrong target left link: %v", err)
	}
	badPin := pin
	badPin.PinID = id + "-missing-pin"
	if err = s.experimentalBeginTargetGuard(ctx, in, badPin); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("missing pin: %v", err)
	}
	if _, err = s.GetProjectionMutationMany(ctx, in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial ledger: %v", err)
	}
	if _, err = s.GetTargetIntentStatus(ctx, in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial target intent: %v", err)
	}
	// Force a failure after target insertion, checking both operation rows roll back.
	if _, err = s.db.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0) ON CONFLICT(calendar_id) DO NOTHING`), target); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_intents(calendar_id,mutation_id,started_at) VALUES(?,?,?)`), target, ":"+in.OperationID, now); err != nil {
		t.Fatal(err)
	}
	if err = s.experimentalBeginTargetGuard(ctx, in, pin); err == nil {
		t.Fatal("duplicate target guard accepted")
	}
	if _, err = s.GetProjectionMutationMany(ctx, in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late failure left ledger: %v", err)
	}
	if _, err = s.GetTargetIntentStatus(ctx, in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late failure left target intent: %v", err)
	}
	if err = s.experimentalBeginExclusiveTargetGuard(ctx, in, pin, id+"-late-target"); err == nil {
		t.Fatal("combined late target guard conflict accepted")
	}
	var lateReservation int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM exclusive_dispatch_reservations WHERE operation_id=?`), in.OperationID).Scan(&lateReservation); err != nil || lateReservation != 0 {
		t.Fatalf("combined late failure left reservation: %d %v", lateReservation, err)
	}
	if _, err = s.GetProjectionMutationMany(ctx, in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("combined late failure left ledger: %v", err)
	}
	if _, err = s.GetTargetIntentStatus(ctx, in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("combined late failure left intent: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_intents WHERE calendar_id=? AND mutation_id=?`), target, ":"+in.OperationID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		id  string
		err error
	}
	ch := make(chan result, 2)
	for i := 0; i < 2; i++ {
		req := in
		req.OperationID = fmt.Sprintf("%s-operation-%d", id, i)
		go func() { ch <- result{req.OperationID, s.experimentalBeginTargetGuard(ctx, req, pin)} }()
	}
	var winner string
	success, blocked := 0, 0
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err == nil {
			success++
			winner = r.id
		} else if errors.Is(r.err, ErrTargetIntentBlocked) {
			blocked++
		} else {
			t.Fatal(r.err)
		}
	}
	if success != 1 || blocked != 1 {
		t.Fatalf("atomic race: %d %d", success, blocked)
	}
	state, err := s.GetTargetIntentStatus(ctx, winner)
	if err != nil || state.Status != "pending" {
		t.Fatalf("target status: %v", err)
	}
	guard, err := s.InspectProjectionMutations(ctx, target)
	if err != nil || guard.Pending != 1 {
		t.Fatalf("target guard: %+v %v", guard, err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operation_members WHERE operation_id=? AND calendar_id=?`), winner, target); err == nil {
		t.Fatal("link failed to protect target guard member")
	}
	// Existing public resolution must fail and roll back its cursor changes while
	// this link exists, regardless of the target intent's unknown outcome.
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, []string{target}, winner, now); err == nil {
		t.Fatal("guard resolved without target completion")
	}
	// A receipt is an unverified caller assertion, not proof that Google
	// committed the request. It must not make this guarded operation eligible
	// for reconciliation, even when all receipt fields match the intent.
	receipt := TargetOperationReceipt{OperationID: winner, RequestHash: in.PayloadHash, Kind: "create", TargetCalendarID: target, TargetEventID: "abcde12345", OwnershipMarker: in.OwnershipMarker, ResponseETag: "synthetic-etag", RecordedAt: now}
	if err = s.RecordTargetOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, []string{target}, winner, now); err == nil {
		t.Fatal("unverified receipt released the target guard")
	}
	var cursor, statusAfter string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status FROM calendar_sync_state WHERE calendar_id=?`), target).Scan(&cursor, &statusAfter); err != nil || cursor != "old-cursor" || statusAfter != "ready" {
		t.Fatalf("blocked resolve changed sync state: %v", err)
	}
	guard, err = s.InspectProjectionMutations(ctx, target)
	if err != nil || guard.Pending != 1 {
		t.Fatalf("guard lost after rejected resolution: %+v %v", guard, err)
	}
	in.OperationID = winner
	if err = s.experimentalBeginTargetGuard(ctx, in, pin); err == nil {
		t.Fatal("duplicate operation accepted")
	}
	if err = s.MarkTargetIntentAmbiguous(ctx, winner, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, []string{target}, winner, now); err == nil {
		t.Fatal("ambiguous outcome released the target guard")
	}
	guard, err = s.InspectProjectionMutations(ctx, target)
	if err != nil || guard.Pending != 1 {
		t.Fatalf("guard lost after ambiguous outcome: %+v %v", guard, err)
	}
	in.OperationID = id + "-retry-operation"
	if err = s.experimentalBeginTargetGuard(ctx, in, pin); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("ambiguous retry accepted: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=? WHERE id=?`), "UTC", source); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET cursor=?,status=?,last_success_at=? WHERE calendar_id=?`), "synthetic", "ready", now, source); err != nil {
		t.Fatal(err)
	}
	readinessCases := []struct {
		name, change, restore string
		args, restoreArgs     []any
	}{
		{"disconnected", `UPDATE connections SET status=? WHERE id=?`, `UPDATE connections SET status=? WHERE id=?`, []any{"error", id}, []any{"connected", id}},
		{"wrong provider", `UPDATE connections SET provider=? WHERE id=?`, `UPDATE connections SET provider=? WHERE id=?`, []any{"microsoft", id}, []any{"google", id}},
		{"syncing", `UPDATE calendar_sync_state SET status=? WHERE calendar_id=?`, `UPDATE calendar_sync_state SET status=? WHERE calendar_id=?`, []any{"syncing", source}, []any{"ready", source}},
		{"cursor mismatch", `UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`, `UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`, []any{"different", source}, []any{"synthetic", source}},
		{"empty cursor", `UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`, `UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`, []any{"", source}, []any{"synthetic", source}},
		{"timezone mismatch", `UPDATE calendars SET timezone=? WHERE id=?`, `UPDATE calendars SET timezone=? WHERE id=?`, []any{"Europe/Belgrade", source}, []any{"UTC", source}},
		{"window mismatch", `UPDATE calendar_sync_state SET window_start=? WHERE calendar_id=?`, `UPDATE calendar_sync_state SET window_start=? WHERE calendar_id=?`, []any{start.Add(time.Minute), source}, []any{start, source}},
		{"missing success", `UPDATE calendar_sync_state SET last_success_at=NULL WHERE calendar_id=?`, `UPDATE calendar_sync_state SET last_success_at=? WHERE calendar_id=?`, []any{source}, []any{now, source}},
		{"stale success", `UPDATE calendar_sync_state SET last_success_at=? WHERE calendar_id=?`, `UPDATE calendar_sync_state SET last_success_at=? WHERE calendar_id=?`, []any{now.Add(-time.Second), source}, []any{now, source}},
		{"missing job deadline", `UPDATE sync_jobs SET lease_until=NULL WHERE id=?`, `UPDATE sync_jobs SET lease_until=? WHERE id=?`, []any{id}, []any{now.Add(JobLeaseDuration), id}},
		{"expired job deadline", `UPDATE sync_jobs SET lease_until=? WHERE id=?`, `UPDATE sync_jobs SET lease_until=? WHERE id=?`, []any{now.Add(-time.Second), id}, []any{now.Add(JobLeaseDuration), id}},
		{"pin beyond job deadline", `UPDATE sync_jobs SET lease_until=? WHERE id=?`, `UPDATE sync_jobs SET lease_until=? WHERE id=?`, []any{now.Add(10 * time.Second), id}, []any{now.Add(JobLeaseDuration), id}},
	}
	for n, tc := range readinessCases {
		if _, err = s.db.ExecContext(ctx, s.query(tc.change), tc.args...); err != nil {
			t.Fatal(err)
		}
		candidate := in
		candidate.OperationID = fmt.Sprintf("%s-readiness-%d", id, n)
		candidate.IdentityHash = stagingDigest(tc.name)
		if err = s.experimentalBeginExclusiveTargetGuard(ctx, candidate, pin, id+"-readiness-target"); !errors.Is(err, ErrTargetIntentBlocked) {
			t.Fatalf("%s accepted: %v", tc.name, err)
		}
		if _, err = s.GetProjectionMutationMany(ctx, candidate.OperationID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s left ledger: %v", tc.name, err)
		}
		if _, err = s.db.ExecContext(ctx, s.query(tc.restore), tc.restoreArgs...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.ExecContext(ctx, s.query(`INSERT INTO calendar_sync_quarantine(calendar_id,object_id,error_code,first_seen_at,last_seen_at,next_repair_at,active) VALUES(?,?,?,?,?,?,?)`), source, "synthetic", "protocol", now, now, now, true); err != nil {
		t.Fatal(err)
	}
	quarantine := in
	quarantine.OperationID = id + "-quarantine-operation"
	quarantine.IdentityHash = stagingDigest("quarantine")
	if err = s.experimentalBeginExclusiveTargetGuard(ctx, quarantine, pin, id+"-readiness-target"); !errors.Is(err, ErrTargetIntentBlocked) {
		t.Fatalf("active quarantine accepted: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`DELETE FROM calendar_sync_quarantine WHERE calendar_id=?`), source); err != nil {
		t.Fatal(err)
	}
	// Different occurrence identities sharing an opaque target key must produce
	// one complete combined operation and one complete rollback.
	combined := in
	combined.IdentityHash = stagingDigest("combined-identity")
	combined.OperationID = id + "-combined-operation"
	if err = s.experimentalBeginExclusiveTargetGuard(ctx, combined, pin, id+"-opaque-target"); err != nil {
		t.Fatalf("combined begin: %v", err)
	}
	var reservationLinks int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM exclusive_dispatch_guard_members WHERE operation_id=? AND calendar_id=?`), combined.OperationID, target).Scan(&reservationLinks); err != nil || reservationLinks != 1 {
		t.Fatalf("combined guard link: %d %v", reservationLinks, err)
	}
	conflict := combined
	conflict.OperationID = id + "-combined-conflict"
	conflict.IdentityHash = stagingDigest("another-identity")
	if err = s.experimentalBeginExclusiveTargetGuard(ctx, conflict, pin, id+"-opaque-target"); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("combined duplicate target: %v", err)
	}
	if _, err = s.GetProjectionMutationMany(ctx, conflict.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("combined conflict left ledger: %v", err)
	}
	if _, err = s.GetTargetIntentStatus(ctx, conflict.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("combined conflict left target intent: %v", err)
	}
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, []string{target}, combined.OperationID, now); err == nil {
		t.Fatal("combined fence was resolved without completion")
	}
	if err = s.experimentalReleaseExclusiveAfterConfirmedRun(ctx, combined.OperationID, []string{target}, now); err == nil {
		t.Fatal("run release bypassed the linked target intent")
	}
	if err = s.experimentalBeginExclusiveTargetGuard(ctx, conflict, pin, id+"-opaque-target"); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("blocked release freed reservation: %v", err)
	}
}
