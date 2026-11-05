package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func ensureProjectionCheckpointTestSchema(t *testing.T, s *Store) {
	t.Helper()
	q := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='projection_rule_checkpoints')`
	if s.dialect == DialectPostgres {
		q = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='projection_rule_checkpoints')`
	}
	var found bool
	if err := s.db.QueryRowContext(t.Context(), q).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found {
		return
	}
	script, err := migrations.ReadFile(fmt.Sprintf("migrations/%s/019_projection_checkpoints.sql", s.dialect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteProjectionCheckpointContract(t *testing.T) {
	s := newSQLiteStore(t)
	ensureSnapshotPinTestSchema(t, s)
	ensureProjectionCheckpointTestSchema(t, s)
	runProjectionCheckpointContract(t, s)
}
func runProjectionCheckpointContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fmt.Sprintf("checkpoint-test-%d", now.UnixNano())
	source, target := id+"-source", id+"-target"
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_rule_checkpoints WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_snapshot_pins WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, cal := range []string{source, target} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: cal, ConnectionID: id, ProviderCalendarID: cal, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	for rev := int64(0); rev < 2; rev++ {
		stage := ProjectionSnapshotInput{CalendarID: source, StageID: fmt.Sprintf("checkpoint_stage_identifier_%d", rev), Timezone: "UTC", BaseRevision: &rev, WindowStart: start, WindowEnd: end, CompletedAt: now, Cursor: fmt.Sprintf("cursor-%d", rev), Payload: []byte(`[]`), Complete: true}
		if err := s.StageProjectionSnapshot(ctx, stage); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PublishProjectionSnapshot(ctx, source, stage.StageID); err != nil {
			t.Fatal(err)
		}
		if rev == 0 {
			seedVerifiedProjectionSnapshot(t, s, source, rev+1)
		}
	}
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
	pin := SnapshotPinRequest{PinID: "checkpoint_pin_identifier_1", RuleID: id, JobID: id, WorkerID: "worker", CalendarID: source, RuleVersion: 1, Revision: 1, JobAttempt: job.Attempt, Now: now, ExpiresAt: now.Add(30 * time.Second), LeaseDuration: time.Minute}
	// A previously persisted pin must not bypass a later unverified check.
	_, err = s.db.ExecContext(ctx, s.query(`INSERT INTO projection_snapshot_pins(pin_id,calendar_id,revision,rule_id,rule_version,job_id,job_attempt,worker_id,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), pin.PinID, source, 2, id, pin.RuleVersion, id, pin.JobAttempt, pin.WorkerID, "active", now, pin.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	unverified := pin
	unverified.Revision = 2
	if err = s.RecordProjectionCheckpoint(ctx, ProjectionCheckpointInput{Pin: unverified, WindowStart: start, WindowEnd: end}); !errors.Is(err, ErrProjectionCheckpointConflict) {
		t.Fatalf("unverified revision: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_snapshot_pins WHERE pin_id=?`), pin.PinID); err != nil {
		t.Fatal(err)
	}
	seedVerifiedProjectionSnapshot(t, s, source, 2)
	if err = s.AcquireSnapshotPin(ctx, pin); err != nil {
		t.Fatal(err)
	}
	in := ProjectionCheckpointInput{Pin: pin, ExpectedVersion: 0, WindowStart: start, WindowEnd: end}
	bad := in
	bad.WindowStart = start.Add(-time.Second)
	if err = s.RecordProjectionCheckpoint(ctx, bad); !errors.Is(err, ErrProjectionCheckpointConflict) {
		t.Fatalf("coverage: %v", err)
	}
	bad = in
	bad.Pin.JobAttempt++
	if err = s.RecordProjectionCheckpoint(ctx, bad); !errors.Is(err, ErrProjectionCheckpointConflict) {
		t.Fatalf("stale job: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET kind=? WHERE id=?`), "dry_run", id); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordProjectionCheckpoint(ctx, in); !errors.Is(err, ErrProjectionCheckpointConflict) {
		t.Fatalf("dry run advanced checkpoint: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET kind=? WHERE id=?`), "manual", id); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- s.RecordProjectionCheckpoint(ctx, in) }()
	}
	ok, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			ok++
		} else if errors.Is(err, ErrProjectionCheckpointConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", ok, conflict)
	}
	checkpoint, err := s.GetProjectionCheckpoint(ctx, id)
	if err != nil || checkpoint.CheckpointVersion != 1 || checkpoint.Revision != 1 || !checkpoint.WindowStart.Equal(start) || !checkpoint.WindowEnd.Equal(end) {
		t.Fatalf("checkpoint: %+v %v", checkpoint, err)
	}
	if err = s.ReleaseSnapshotPin(ctx, pin); err != nil {
		t.Fatal(err)
	}
	in.ExpectedVersion = 1
	if err = s.RecordProjectionCheckpoint(ctx, in); !errors.Is(err, ErrProjectionCheckpointConflict) {
		t.Fatalf("released pin accepted: %v", err)
	}
	// A new job attempt may advance to the next immutable revision.
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET attempt=?,claimed_at=? WHERE id=?`), 2, now, id); err != nil {
		t.Fatal(err)
	}
	pin.PinID = "checkpoint_pin_identifier_2"
	pin.JobAttempt = 2
	pin.Revision = 2
	if err = s.AcquireSnapshotPin(ctx, pin); err != nil {
		t.Fatal(err)
	}
	in.Pin = pin
	if err = s.RecordProjectionCheckpoint(ctx, in); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = s.GetProjectionCheckpoint(ctx, id)
	if err != nil || checkpoint.CheckpointVersion != 2 || checkpoint.Revision != 2 {
		t.Fatalf("advanced: %+v %v", checkpoint, err)
	}
	if err = s.RecordProjectionCheckpoint(ctx, in); !errors.Is(err, ErrProjectionCheckpointConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	// Revision dependencies are retained even if a later pin expires or releases.
	if _, err = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_snapshots WHERE calendar_id=? AND revision=?`), source, 2); err == nil {
		t.Fatal("deleted checkpoint revision")
	}
}
