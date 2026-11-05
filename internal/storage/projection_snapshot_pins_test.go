package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func ensureSnapshotPinTestSchema(t *testing.T, s *Store) {
	t.Helper()
	q := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='projection_snapshot_pins')`
	if s.dialect == DialectPostgres {
		q = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='projection_snapshot_pins')`
	}
	var exists bool
	if err := s.db.QueryRowContext(t.Context(), q).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		return
	}
	script, err := migrations.ReadFile(fmt.Sprintf("migrations/%s/017_snapshot_pins.sql", s.dialect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteSnapshotPinContract(t *testing.T) {
	s := newSQLiteStore(t)
	ensureSnapshotPinTestSchema(t, s)
	runSnapshotPinContract(t, s)
}
func runSnapshotPinContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fmt.Sprintf("pin-test-%d", now.UnixNano())
	source, target := id+"-source", id+"-target"
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
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
	for revision := int64(0); revision < 2; revision++ {
		stage := ProjectionSnapshotInput{CalendarID: source, StageID: fmt.Sprintf("pin_stage_identifier_%d", revision), Timezone: "UTC", BaseRevision: &revision, GuardEpoch: 0, WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(time.Hour), CompletedAt: now, Cursor: fmt.Sprintf("cursor-%d", revision), Payload: []byte(`[]`), Complete: true}
		if err := s.StageProjectionSnapshot(ctx, stage); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PublishProjectionSnapshot(ctx, source, stage.StageID); err != nil {
			t.Fatal(err)
		}
		if revision == 0 {
			seedVerifiedProjectionSnapshot(t, s, source, revision+1)
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
	pin := SnapshotPinRequest{PinID: "snapshot_pin_identifier_1", RuleID: id, JobID: id, WorkerID: "worker", CalendarID: source, RuleVersion: 1, Revision: 1, JobAttempt: job.Attempt, Now: now, ExpiresAt: now.Add(30*time.Second + 999*time.Nanosecond), LeaseDuration: time.Minute}
	wrong := pin
	wrong.Revision = 2
	if err := s.AcquireSnapshotPin(ctx, wrong); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("unverified revision: %v", err)
	}
	seedVerifiedProjectionSnapshot(t, s, source, 2)
	wrong = pin
	wrong.RuleVersion++
	if err := s.AcquireSnapshotPin(ctx, wrong); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("version: %v", err)
	}
	wrong = pin
	wrong.JobAttempt++
	if err := s.AcquireSnapshotPin(ctx, wrong); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("attempt: %v", err)
	}
	wrong = pin
	wrong.Revision = 99
	if err := s.AcquireSnapshotPin(ctx, wrong); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("missing revision: %v", err)
	}
	results := make(chan struct {
		id  string
		err error
	}, 2)
	for i := 0; i < 2; i++ {
		p := pin
		p.PinID = fmt.Sprintf("snapshot_pin_identifier_%d", i)
		go func() {
			results <- struct {
				id  string
				err error
			}{p.PinID, s.AcquireSnapshotPin(ctx, p)}
		}()
	}
	var chosen string
	success, blocked := 0, 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil {
			success++
			chosen = r.id
		} else if errors.Is(r.err, ErrSnapshotPinBlocked) {
			blocked++
		} else {
			t.Fatal(r.err)
		}
	}
	if success != 1 || blocked != 1 {
		t.Fatalf("pin race: %d %d", success, blocked)
	}
	pin.PinID = chosen
	// Revision 1 stays retained even after revision 2 was published.
	if _, err := s.GetProjectionSnapshot(ctx, source, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`DELETE FROM projection_snapshots WHERE calendar_id=? AND revision=?`), source, 1); err == nil {
		t.Fatal("deleted a pinned revision")
	}
	wrong = pin
	wrong.WorkerID = "other"
	if err := s.ReleaseSnapshotPin(ctx, wrong); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("wrong owner release: %v", err)
	}
	if err := s.ReleaseSnapshotPin(ctx, pin); err != nil {
		t.Fatal(err)
	}
	state, err := s.GetSnapshotPin(ctx, pin.PinID)
	if err != nil || state.Status != "released" || !state.ExpiresAt.Equal(pin.ExpiresAt.Truncate(time.Microsecond)) {
		t.Fatalf("release status: %v", err)
	}
	if err := s.AcquireSnapshotPin(ctx, pin); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("replayed pin: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`DELETE FROM projection_snapshots WHERE calendar_id=? AND revision=?`), source, 1); err == nil {
		t.Fatal("deleted retained operation evidence")
	}
	// Reclaim creates a new attempt; an expired pin cannot be renewed or
	// released by either the old attempt or the current worker.
	fresh := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET attempt=?,claimed_at=? WHERE id=?`), 2, fresh, id); err != nil {
		t.Fatal(err)
	}
	next := pin
	next.PinID = "snapshot_pin_identifier_expiry"
	next.JobAttempt = 2
	next.Now = fresh
	next.ExpiresAt = fresh.Add(200 * time.Millisecond)
	if err := s.AcquireSnapshotPin(ctx, next); err != nil {
		t.Fatal(err)
	}
	old := next
	old.JobAttempt = 1
	if err := s.ReleaseSnapshotPin(ctx, old); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("stale attempt: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := s.ReleaseSnapshotPin(ctx, next); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("expired pin release: %v", err)
	}
	if err := s.AcquireSnapshotPin(ctx, next); !errors.Is(err, ErrSnapshotPinBlocked) {
		t.Fatalf("expired pin replay: %v", err)
	}
}
