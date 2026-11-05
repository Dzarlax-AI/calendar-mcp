package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const maxProjectionSnapshotBytes = 32 << 20
const maxProjectionObservationBytes = 4 << 20
const maxProjectionSnapshotRevisions = 2

var (
	ErrProjectionSnapshotStale            = errors.New("projection snapshot guard changed or mutation is unresolved")
	ErrProjectionSnapshotLimit            = errors.New("projection snapshot revision capacity exhausted")
	ErrProjectionSnapshotAlreadyPublished = errors.New("projection snapshot operation already published")
)

// ProjectionSnapshotInput is an unverified caller assertion of a complete
// snapshot. It is not proof of provider completeness, health, or freshness.
// StageID must be unique, random and opaque, with no sensitive data.
type ProjectionSnapshotInput struct {
	CalendarID, StageID, Timezone       string
	GuardEpoch                          int64
	BaseRevision                        *int64
	WindowStart, WindowEnd, CompletedAt time.Time
	Cursor                              string `json:"-"`
	Payload                             []byte `json:"-"`
	Complete                            bool
}

type ProjectionSnapshot struct {
	CalendarID, StageID, Timezone       string
	Revision, GuardEpoch, BaseRevision  int64
	Verified                            bool
	WindowStart, WindowEnd, CompletedAt time.Time
	Cursor                              string `json:"-"`
	Payload                             []byte `json:"-"`
}

// StageProjectionSnapshot stores one bounded candidate per calendar. The
// caller must build a complete, consistent snapshot; this method cannot verify
// its provenance. It never changes the existing cache or publishes a revision.
func (s *Store) StageProjectionSnapshot(ctx context.Context, in ProjectionSnapshotInput) error {
	if _, err := time.LoadLocation(in.Timezone); in.Timezone == "" || in.Timezone == "Local" || err != nil {
		return errors.New("invalid snapshot calendar time zone")
	}
	if in.CalendarID == "" || !validProjectionMutationID(in.StageID) || in.GuardEpoch < 0 || in.BaseRevision == nil || *in.BaseRevision < 0 || !in.Complete || in.WindowStart.IsZero() || !in.WindowEnd.After(in.WindowStart) || in.CompletedAt.IsZero() || len(in.Cursor) > 4096 || len(in.Payload) > maxProjectionSnapshotBytes || !json.Valid(in.Payload) {
		return errors.New("invalid projection snapshot candidate")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin snapshot stage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id, epoch) VALUES (?, 0) ON CONFLICT(calendar_id) DO NOTHING`), in.CalendarID); err != nil {
		return fmt.Errorf("initialize snapshot guard: %w", err)
	}
	if err = lockProjectionSnapshotGuard(ctx, s, tx, in.CalendarID, in.GuardEpoch, *in.BaseRevision); err != nil {
		return err
	}
	var published bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_snapshots WHERE calendar_id=? AND stage_id=?)`), in.CalendarID, in.StageID).Scan(&published); err != nil {
		return fmt.Errorf("check snapshot operation: %w", err)
	}
	if published {
		return ErrProjectionSnapshotAlreadyPublished
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_snapshot_staging(calendar_id,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload) VALUES(?,?,?,?,?,?,?,?,?,?)`), in.CalendarID, in.StageID, in.GuardEpoch, *in.BaseRevision, in.WindowStart.UTC(), in.WindowEnd.UTC(), in.CompletedAt.UTC(), in.Timezone, in.Cursor, in.Payload)
	if err != nil {
		return fmt.Errorf("store snapshot stage: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot stage: %w", err)
	}
	return nil
}

// PublishProjectionSnapshot atomically publishes the staged assertion if the
// guard is unchanged and no mutation intent is pending. Existing revisions are
// never evicted; capacity exhaustion fails closed. This is not connected to the
// sync cursor, lease, quarantine, source health, or production read model.
func (s *Store) PublishProjectionSnapshot(ctx context.Context, calendarID, stageID string) (int64, error) {
	if calendarID == "" || !validProjectionMutationID(stageID) {
		return 0, errors.New("invalid projection snapshot identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin snapshot publication: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Lock before reading stage, epoch or capacity. All mutation writers take the
	// same row lock, and no network operation occurs under this transaction.
	if err = lockProjectionGuardRow(ctx, s, tx, calendarID); err != nil {
		return 0, err
	}
	var published bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_snapshots WHERE calendar_id=? AND stage_id=?)`), calendarID, stageID).Scan(&published); err != nil {
		return 0, fmt.Errorf("check snapshot operation: %w", err)
	}
	if published {
		return 0, ErrProjectionSnapshotAlreadyPublished
	}
	var epoch, stageEpoch, revision, baseRevision, pending, count int64
	err = tx.QueryRowContext(ctx, s.query(`SELECT g.epoch,g.revision,st.guard_epoch,st.base_revision,(SELECT COUNT(*) FROM projection_mutation_intents i WHERE i.calendar_id=g.calendar_id),(SELECT COUNT(*) FROM projection_snapshots p WHERE p.calendar_id=g.calendar_id) FROM projection_mutation_guards g JOIN projection_snapshot_staging st ON st.calendar_id=g.calendar_id WHERE g.calendar_id=? AND st.stage_id=?`), calendarID, stageID).Scan(&epoch, &revision, &stageEpoch, &baseRevision, &pending, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read snapshot publication state: %w", err)
	}
	if epoch != stageEpoch || revision != baseRevision || pending != 0 {
		return 0, ErrProjectionSnapshotStale
	}
	if count >= maxProjectionSnapshotRevisions {
		return 0, ErrProjectionSnapshotLimit
	}
	if revision == int64(^uint64(0)>>1) {
		return 0, errors.New("projection snapshot revision exhausted")
	}
	revision++
	res, err := tx.ExecContext(ctx, s.query(`INSERT INTO projection_snapshots(calendar_id,revision,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload) SELECT calendar_id,?,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload FROM projection_snapshot_staging WHERE calendar_id=? AND stage_id=?`), revision, calendarID, stageID)
	if err != nil {
		return 0, fmt.Errorf("publish snapshot data: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n != 1 {
		return 0, ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_guards SET revision=? WHERE calendar_id=?`), revision, calendarID); err != nil {
		return 0, fmt.Errorf("advance snapshot revision: %w", err)
	}
	if _, err = tx.ExecContext(ctx, s.query(`DELETE FROM projection_snapshot_staging WHERE calendar_id=? AND stage_id=?`), calendarID, stageID); err != nil {
		return 0, fmt.Errorf("remove published snapshot stage: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit snapshot publication: %w", err)
	}
	return revision, nil
}

// DiscardProjectionSnapshotStage removes only the named candidate, not any
// published revision. It is useful after a stale candidate or failed attempt.
func (s *Store) DiscardProjectionSnapshotStage(ctx context.Context, calendarID, stageID string) error {
	if calendarID == "" || !validProjectionMutationID(stageID) {
		return errors.New("invalid projection snapshot identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin discard snapshot stage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProjectionGuardRow(ctx, s, tx, calendarID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`DELETE FROM projection_snapshot_staging WHERE calendar_id=? AND stage_id=?`), calendarID, stageID)
	if err != nil {
		return fmt.Errorf("discard snapshot stage: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit discard snapshot stage: %w", err)
	}
	return nil
}

// GetProjectionSnapshot fetches an exact immutable revision. It provides no
// readiness or freshness guarantee, and revision retention is not a pin API.
func (s *Store) GetProjectionSnapshot(ctx context.Context, calendarID string, revision int64) (ProjectionSnapshot, error) {
	var out ProjectionSnapshot
	if calendarID == "" || revision < 1 {
		return out, errors.New("invalid projection snapshot reference")
	}
	out.CalendarID = calendarID
	out.Revision = revision
	err := s.db.QueryRowContext(ctx, s.query(`SELECT stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload,verified FROM projection_snapshots WHERE calendar_id=? AND revision=?`), calendarID, revision).Scan(&out.StageID, &out.GuardEpoch, &out.BaseRevision, &out.WindowStart, &out.WindowEnd, &out.CompletedAt, &out.Timezone, &out.Cursor, &out.Payload, &out.Verified)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionSnapshot{}, ErrNotFound
	}
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("read snapshot revision: %w", err)
	}
	return out, nil
}

// GetProjectionSnapshotByStageID resolves an ambiguous publication commit.
// Absence does not prove a transaction in progress will not later commit.
func (s *Store) GetProjectionSnapshotByStageID(ctx context.Context, calendarID, stageID string) (ProjectionSnapshot, error) {
	var out ProjectionSnapshot
	if calendarID == "" || !validProjectionMutationID(stageID) {
		return out, errors.New("invalid projection snapshot identity")
	}
	out.CalendarID, out.StageID = calendarID, stageID
	err := s.db.QueryRowContext(ctx, s.query(`SELECT revision,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload,verified FROM projection_snapshots WHERE calendar_id=? AND stage_id=?`), calendarID, stageID).Scan(&out.Revision, &out.GuardEpoch, &out.BaseRevision, &out.WindowStart, &out.WindowEnd, &out.CompletedAt, &out.Timezone, &out.Cursor, &out.Payload, &out.Verified)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionSnapshot{}, ErrNotFound
	}
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("read snapshot operation: %w", err)
	}
	return out, nil
}

func lockProjectionGuardRow(ctx context.Context, s *Store, tx *sql.Tx, calendarID string) error {
	res, err := tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_guards SET epoch=epoch WHERE calendar_id=?`), calendarID)
	if err != nil {
		return fmt.Errorf("lock projection guard: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
func lockProjectionSnapshotGuard(ctx context.Context, s *Store, tx *sql.Tx, calendarID string, expectedEpoch, expectedRevision int64) error {
	if err := lockProjectionGuardRow(ctx, s, tx, calendarID); err != nil {
		return err
	}
	var epoch, revision, pending int64
	err := tx.QueryRowContext(ctx, s.query(`SELECT epoch,revision,(SELECT COUNT(*) FROM projection_mutation_intents WHERE calendar_id=?) FROM projection_mutation_guards WHERE calendar_id=?`), calendarID, calendarID).Scan(&epoch, &revision, &pending)
	if err != nil {
		return fmt.Errorf("read projection guard: %w", err)
	}
	if epoch != expectedEpoch || revision != expectedRevision || pending != 0 {
		return ErrProjectionSnapshotStale
	}
	return nil
}
