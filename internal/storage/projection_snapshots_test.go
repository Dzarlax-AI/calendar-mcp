package storage

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestProjectionSnapshotsSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	runProjectionSnapshotsContract(t, s, "calendar", now)
}

func runProjectionSnapshotsContract(t *testing.T, s *Store, calendarID string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	base := int64(0)
	stage := ProjectionSnapshotInput{Timezone: "Europe/Belgrade", CalendarID: calendarID, StageID: "stage-0123456789abcdef", BaseRevision: &base, WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(time.Hour), CompletedAt: now, Cursor: "opaque", Payload: []byte(`[{"id":"one"}]`), Complete: true}
	bad := stage
	bad.Timezone = "Local"
	if err := s.StageProjectionSnapshot(ctx, bad); err == nil {
		t.Fatal("accepted host-local time zone")
	}
	bad = stage
	bad.BaseRevision = nil
	if err := s.StageProjectionSnapshot(ctx, bad); err == nil {
		t.Fatal("accepted missing base revision")
	}
	bad = stage
	bad.Complete = false
	if err := s.StageProjectionSnapshot(ctx, bad); err == nil {
		t.Fatal("accepted incomplete")
	}
	bad = stage
	bad.Payload = bytes.Repeat([]byte("a"), maxProjectionSnapshotBytes+1)
	if err := s.StageProjectionSnapshot(ctx, bad); err == nil {
		t.Fatal("accepted oversized")
	}
	bad = stage
	bad.WindowEnd = bad.WindowStart
	if err := s.StageProjectionSnapshot(ctx, bad); err == nil {
		t.Fatal("accepted empty window")
	}
	if err := s.StageProjectionSnapshot(ctx, stage); err != nil {
		t.Fatal(err)
	}
	// A candidate from before a mutation cannot be published even if the intent
	// was subsequently resolved; its epoch is no longer current.
	if err := s.beginProjectionMutation(ctx, calendarID, "mutation-0123456789", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, calendarID, stage.StageID); !errors.Is(err, ErrProjectionSnapshotStale) {
		t.Fatalf("pending publish: %v", err)
	}
	if err := s.resolveProjectionMutation(ctx, calendarID, "mutation-0123456789"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, calendarID, stage.StageID); !errors.Is(err, ErrProjectionSnapshotStale) {
		t.Fatalf("changed epoch publish: %v", err)
	}
	if _, err := s.GetProjectionSnapshot(ctx, calendarID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale published: %v", err)
	}
	if err := s.DiscardProjectionSnapshotStage(ctx, calendarID, stage.StageID); err != nil {
		t.Fatal(err)
	}
	stage.GuardEpoch = 2
	if err := s.StageProjectionSnapshot(ctx, stage); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PublishProjectionSnapshot(ctx, calendarID, stage.StageID); err != nil || n != 1 {
		t.Fatalf("first publication %d %v", n, err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, calendarID, stage.StageID); !errors.Is(err, ErrProjectionSnapshotAlreadyPublished) {
		t.Fatalf("repeat publication: %v", err)
	}
	byStage, err := s.GetProjectionSnapshotByStageID(ctx, calendarID, stage.StageID)
	if err != nil || byStage.Revision != 1 || byStage.BaseRevision != 0 || !bytes.Equal(byStage.Payload, stage.Payload) {
		t.Fatalf("lookup by stage: %+v %v", byStage, err)
	}
	first, err := s.GetProjectionSnapshot(ctx, calendarID, 1)
	if err != nil || !bytes.Equal(first.Payload, stage.Payload) || first.GuardEpoch != 2 || !first.WindowStart.Equal(stage.WindowStart) || !first.WindowEnd.Equal(stage.WindowEnd) || first.Cursor != stage.Cursor {
		t.Fatalf("first snapshot: %+v %v", first, err)
	}
	stage.StageID = "second-0123456789abcdef"
	stage.Payload = []byte(`[{"id":"two"}]`)
	if err := s.StageProjectionSnapshot(ctx, stage); !errors.Is(err, ErrProjectionSnapshotStale) {
		t.Fatalf("stale base: %v", err)
	}
	base = 1
	duplicate := stage
	duplicate.StageID = byStage.StageID
	if err := s.StageProjectionSnapshot(ctx, duplicate); !errors.Is(err, ErrProjectionSnapshotAlreadyPublished) {
		t.Fatalf("reused stage ID: %v", err)
	}
	if err := s.StageProjectionSnapshot(ctx, stage); err != nil {
		t.Fatal(err)
	}
	// Simulate a candidate whose persisted base no longer matches the guard;
	// publication must recheck it rather than relying on staging-time checks.
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE projection_snapshot_staging SET base_revision=? WHERE calendar_id=?`), 0, calendarID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, calendarID, stage.StageID); !errors.Is(err, ErrProjectionSnapshotStale) {
		t.Fatalf("publish stale persisted base: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE projection_snapshot_staging SET base_revision=? WHERE calendar_id=?`), 1, calendarID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PublishProjectionSnapshot(ctx, calendarID, stage.StageID); err != nil || n != 2 {
		t.Fatalf("second publication %d %v", n, err)
	}
	stage.StageID = "third-0123456789abcdef"
	stage.Payload = []byte(`[]`)
	base = 2
	if err := s.StageProjectionSnapshot(ctx, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, calendarID, stage.StageID); !errors.Is(err, ErrProjectionSnapshotLimit) {
		t.Fatalf("capacity: %v", err)
	}
	for _, rev := range []int64{1, 2} {
		if _, err := s.GetProjectionSnapshot(ctx, calendarID, rev); err != nil {
			t.Fatalf("revision %d: %v", rev, err)
		}
	}
	if _, err := s.GetProjectionSnapshot(ctx, calendarID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("third revision: %v", err)
	}
	if err := s.DiscardProjectionSnapshotStage(ctx, calendarID, stage.StageID); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionSnapshotPublicationRollbackSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	base := int64(0)
	in := ProjectionSnapshotInput{Timezone: "Europe/Belgrade", CalendarID: "calendar", StageID: "stage-0123456789abcdef", BaseRevision: &base, WindowStart: now, WindowEnd: now.Add(time.Hour), CompletedAt: now, Payload: []byte(`[]`), Complete: true}
	if err := s.StageProjectionSnapshot(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_stage_delete BEFORE DELETE ON projection_snapshot_staging BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, "calendar", in.StageID); err == nil {
		t.Fatal("publication should roll back")
	}
	if _, err := s.GetProjectionSnapshot(ctx, "calendar", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial revision: %v", err)
	}
	var revision, count int
	if err := s.db.QueryRowContext(ctx, `SELECT revision,(SELECT COUNT(*) FROM projection_snapshot_staging WHERE calendar_id='calendar') FROM projection_mutation_guards WHERE calendar_id='calendar'`).Scan(&revision, &count); err != nil || revision != 0 || count != 1 {
		t.Fatalf("rollback revision=%d stages=%d error=%v", revision, count, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_stage_delete`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PublishProjectionSnapshot(ctx, "calendar", in.StageID); err != nil || n != 1 {
		t.Fatalf("retry publish %d %v", n, err)
	}
}

func TestProjectionSnapshotMutationRaceSQLite(t *testing.T) {
	first, now := newEventCacheStore(t)
	var seq int
	var name, path string
	if err := first.db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runProjectionSnapshotMutationRace(t, first, second, "calendar", now)
}

func runProjectionSnapshotMutationRace(t *testing.T, first, second *Store, calendarID string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	base := int64(0)
	in := ProjectionSnapshotInput{Timezone: "Europe/Belgrade", CalendarID: calendarID, StageID: "stage-0123456789abcdef", BaseRevision: &base, WindowStart: now, WindowEnd: now.Add(time.Hour), CompletedAt: now, Payload: []byte(`[]`), Complete: true}
	if err := first.StageProjectionSnapshot(ctx, in); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	publish := make(chan error, 1)
	mutation := make(chan error, 1)
	go func() {
		<-start
		_, err := first.PublishProjectionSnapshot(ctx, calendarID, in.StageID)
		publish <- err
	}()
	go func() {
		<-start
		mutation <- second.beginProjectionMutation(ctx, calendarID, "mutation-0123456789", now)
	}()
	close(start)
	mutErr, pubErr := <-mutation, <-publish
	if mutErr != nil {
		t.Fatalf("mutation failed: %v", mutErr)
	}
	if pubErr != nil && !errors.Is(pubErr, ErrProjectionSnapshotStale) {
		t.Fatalf("publication error: %v", pubErr)
	}
	state, err := first.InspectProjectionMutations(ctx, calendarID)
	if err != nil || state.Epoch != 1 || state.Pending != 1 {
		t.Fatalf("guard after race: %+v %v", state, err)
	}
	snap, err := first.GetProjectionSnapshot(ctx, calendarID, 1)
	if pubErr == nil {
		if err != nil || snap.GuardEpoch != 0 {
			t.Fatalf("published revision: %+v %v", snap, err)
		}
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale revision exists: %v", err)
	}
}
