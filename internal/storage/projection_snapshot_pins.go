package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

var ErrSnapshotPinBlocked = errors.New("snapshot pin unavailable or fence changed")

const maxSnapshotPinsPerRule = 1024

// SnapshotPinRequest pins a persisted immutable revision for one job attempt.
// It is a retention claim only: it does not prove source readiness, freshness,
// provider ownership, or permission to make any external write. LeaseDuration
// must come from trusted server-side worker policy; the job table does not
// persist lease_until, so caller-supplied durations cannot prove a current lease.
// ExpiresAt is rounded down to microseconds in acquire and release.
type SnapshotPinRequest struct {
	PinID, RuleID, JobID, WorkerID, CalendarID string
	RuleVersion, Revision                      int64
	JobAttempt                                 int
	Now, ExpiresAt                             time.Time
	LeaseDuration                              time.Duration
}
type SnapshotPinState struct {
	PinID, Status, RuleID, JobID, WorkerID, CalendarID string
	RuleVersion, Revision                              int64
	JobAttempt                                         int
	CreatedAt, ExpiresAt                               time.Time
}

func validPinRequest(p SnapshotPinRequest) bool {
	return validProjectionMutationID(p.PinID) && p.RuleID != "" && len(p.RuleID) <= 512 && p.JobID != "" && len(p.JobID) <= 512 && p.WorkerID != "" && len(p.WorkerID) <= 512 && p.CalendarID != "" && len(p.CalendarID) <= 1024 && p.RuleVersion > 0 && p.Revision > 0 && p.JobAttempt > 0 && !p.Now.IsZero() && !p.ExpiresAt.IsZero() && p.ExpiresAt.After(p.Now) && p.ExpiresAt.Nanosecond()%1000 == 0 && p.LeaseDuration > 0 && p.LeaseDuration <= maxTargetIntentLease && p.ExpiresAt.Sub(p.Now) <= p.LeaseDuration
}

// lockSnapshotPinFence serializes with calendar disable, rule state updates,
// job reclaim, and other pin operations. It deliberately does not assert that
// a source revision is current or complete enough to execute against.
func (s *Store) lockSnapshotPinFence(ctx context.Context, tx *sql.Tx, p SnapshotPinRequest) (time.Time, error) {
	var source, target string
	err := tx.QueryRowContext(ctx, s.query(`SELECT source_calendar_id,target_calendar_id FROM sync_rules WHERE id=?`), p.RuleID).Scan(&source, &target)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (source != p.CalendarID || source == target) {
		return time.Time{}, ErrSnapshotPinBlocked
	}
	if err != nil {
		return time.Time{}, err
	}
	ids := []string{source, target}
	sort.Strings(ids)
	for _, id := range ids {
		if s.dialect == DialectSQLite {
			if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET enabled=enabled WHERE id=?`), id); err != nil {
				return time.Time{}, err
			}
		}
		q := `SELECT enabled FROM calendars WHERE id=?`
		if s.dialect == DialectPostgres {
			q += ` FOR NO KEY UPDATE`
		}
		var enabled bool
		err = tx.QueryRowContext(ctx, s.query(q), id).Scan(&enabled)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !enabled {
			return time.Time{}, ErrSnapshotPinBlocked
		}
		if err != nil {
			return time.Time{}, err
		}
	}
	if err = lockProjectionGuardRow(ctx, s, tx, source); err != nil {
		if errors.Is(err, ErrNotFound) {
			return time.Time{}, ErrSnapshotPinBlocked
		}
		return time.Time{}, err
	}
	q := `SELECT source_calendar_id,target_calendar_id,state,config_version FROM sync_rules WHERE id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var gotSource, gotTarget, state string
	var version int64
	err = tx.QueryRowContext(ctx, s.query(q), p.RuleID).Scan(&gotSource, &gotTarget, &state, &version)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (gotSource != source || gotTarget != target || version != p.RuleVersion || (state != "enabled" && state != "paused")) {
		return time.Time{}, ErrSnapshotPinBlocked
	}
	if err != nil {
		return time.Time{}, err
	}
	q = `SELECT state,kind,claimed_by,attempt,claimed_at FROM sync_jobs WHERE id=? AND rule_id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var jobState, kind string
	var owner sql.NullString
	var attempt int
	var claimed sql.NullTime
	err = tx.QueryRowContext(ctx, s.query(q), p.JobID, p.RuleID).Scan(&jobState, &kind, &owner, &attempt, &claimed)
	now := time.Now().UTC()
	if errors.Is(err, sql.ErrNoRows) || err == nil && (jobState != "running" || !owner.Valid || owner.String != p.WorkerID || attempt != p.JobAttempt || !claimed.Valid || claimed.Time.After(now) || !claimed.Time.Add(p.LeaseDuration).After(now) || p.ExpiresAt.After(claimed.Time.Add(p.LeaseDuration)) || !p.ExpiresAt.After(now) || (state == "paused" && kind != "dry_run")) {
		return time.Time{}, ErrSnapshotPinBlocked
	}
	if err != nil {
		return time.Time{}, err
	}
	return claimed.Time, nil
}

// AcquireSnapshotPin returns an error on every replay, even an identical one.
// An ambiguous commit must be investigated through GetSnapshotPin, not retried.
// NotFound does not prove an in-flight commit cannot still complete; park the
// operation until the original transaction is known to have finished.
// No snapshot/pin garbage collection is implemented; capacity fails closed.
func (s *Store) AcquireSnapshotPin(ctx context.Context, p SnapshotPinRequest) error {
	p.ExpiresAt = p.ExpiresAt.UTC().Truncate(time.Microsecond)
	if !validPinRequest(p) {
		return ErrSnapshotPinBlocked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	claimed, err := s.lockSnapshotPinFence(ctx, tx, p)
	if err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_snapshots WHERE calendar_id=? AND revision=? AND verified=?)`), p.CalendarID, p.Revision, true).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrSnapshotPinBlocked
	}
	var count int
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_snapshot_pins WHERE rule_id=?`), p.RuleID).Scan(&count); err != nil {
		return err
	}
	if count >= maxSnapshotPinsPerRule {
		return ErrSnapshotPinBlocked
	}
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_snapshot_pins WHERE pin_id=? OR (rule_id=? AND job_id=? AND job_attempt=?))`), p.PinID, p.RuleID, p.JobID, p.JobAttempt).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrSnapshotPinBlocked
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_snapshot_pins(pin_id,calendar_id,revision,rule_id,rule_version,job_id,job_attempt,worker_id,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), p.PinID, p.CalendarID, p.Revision, p.RuleID, p.RuleVersion, p.JobID, p.JobAttempt, p.WorkerID, "active", p.Now.UTC(), p.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert snapshot pin: %w", err)
	}
	now := time.Now().UTC()
	if !p.ExpiresAt.After(now) || !claimed.Add(p.LeaseDuration).After(now) {
		return ErrSnapshotPinBlocked
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot pin: %w", err)
	}
	return nil
}

// ReleaseSnapshotPin only releases the exact pin held by the still-current job
// attempt and unchanged rule. It is intentionally not a GC implementation.
func (s *Store) ReleaseSnapshotPin(ctx context.Context, p SnapshotPinRequest) error {
	p.ExpiresAt = p.ExpiresAt.UTC().Truncate(time.Microsecond)
	if !validPinRequest(p) {
		return ErrSnapshotPinBlocked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	claimed, err := s.lockSnapshotPinFence(ctx, tx, p)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, s.query(`UPDATE projection_snapshot_pins SET status='released',released_at=? WHERE pin_id=? AND calendar_id=? AND revision=? AND rule_id=? AND rule_version=? AND job_id=? AND job_attempt=? AND worker_id=? AND status='active' AND expires_at=? AND expires_at>?`), now, p.PinID, p.CalendarID, p.Revision, p.RuleID, p.RuleVersion, p.JobID, p.JobAttempt, p.WorkerID, p.ExpiresAt.UTC(), now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSnapshotPinBlocked
	}
	now = time.Now().UTC()
	if !p.ExpiresAt.After(now) || !claimed.Add(p.LeaseDuration).After(now) {
		return ErrSnapshotPinBlocked
	}
	return tx.Commit()
}

// GetSnapshotPin is a diagnostic point-in-time read, never authorization.
func (s *Store) GetSnapshotPin(ctx context.Context, id string) (SnapshotPinState, error) {
	var p SnapshotPinState
	if !validProjectionMutationID(id) {
		return p, ErrSnapshotPinBlocked
	}
	p.PinID = id
	err := s.db.QueryRowContext(ctx, s.query(`SELECT status,rule_id,job_id,worker_id,calendar_id,rule_version,revision,job_attempt,created_at,expires_at FROM projection_snapshot_pins WHERE pin_id=?`), id).Scan(&p.Status, &p.RuleID, &p.JobID, &p.WorkerID, &p.CalendarID, &p.RuleVersion, &p.Revision, &p.JobAttempt, &p.CreatedAt, &p.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotPinState{}, ErrNotFound
	}
	return p, err
}
