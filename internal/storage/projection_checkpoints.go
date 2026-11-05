package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrProjectionCheckpointConflict = errors.New("projection checkpoint unavailable or fence changed")

// ProjectionCheckpoint is a caller assertion that one frozen window was
// evaluated. It is not proof that target actions succeeded and MUST NOT be used
// as a provider-write authorization or as a garbage-collection decision.
type ProjectionCheckpoint struct {
	RuleID, CalendarID, JobID, WorkerID, PinID string
	CheckpointVersion, Revision, RuleVersion   int64
	JobAttempt                                 int
	WindowStart, WindowEnd, RecordedAt         time.Time
}

// ProjectionCheckpointInput includes an exact pin and expected prior sequence.
// ExpectedVersion must be zero for initial creation, then the observed version
// for every update. A read can help investigate an ambiguous commit, but a
// later writer may already have advanced the row. Without an immutable receipt
// the outcome may remain unknown; never blindly retry or infer success.
// LeaseDuration inside Pin must come from trusted server-side worker policy.
type ProjectionCheckpointInput struct {
	Pin                    SnapshotPinRequest
	ExpectedVersion        int64
	WindowStart, WindowEnd time.Time
}

// RecordProjectionCheckpoint records an isolated, bounded window assertion
// under an active pin, current rule version and claimed job attempt. It rejects
// dry-run jobs, checkpoint-version races, stale pins and backwards revisions.
// The engine must not consume it until it can prove completed target operations.
func (s *Store) RecordProjectionCheckpoint(ctx context.Context, in ProjectionCheckpointInput) error {
	p := in.Pin
	p.ExpiresAt = p.ExpiresAt.UTC().Truncate(time.Microsecond)
	if !validPinRequest(p) || in.ExpectedVersion < 0 || in.ExpectedVersion == int64(^uint64(0)>>1) || in.WindowStart.IsZero() || !in.WindowEnd.After(in.WindowStart) || in.WindowStart.Nanosecond()%1000 != 0 || in.WindowEnd.Nanosecond()%1000 != 0 {
		return ErrProjectionCheckpointConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	claimed, err := s.lockSnapshotPinFence(ctx, tx, p)
	if err != nil {
		if errors.Is(err, ErrSnapshotPinBlocked) {
			return ErrProjectionCheckpointConflict
		}
		return err
	}
	var kind string
	if err = tx.QueryRowContext(ctx, s.query(`SELECT kind FROM sync_jobs WHERE id=? AND rule_id=?`), p.JobID, p.RuleID).Scan(&kind); err != nil {
		return err
	}
	if kind == "dry_run" {
		return ErrProjectionCheckpointConflict
	}
	q := `SELECT status,expires_at FROM projection_snapshot_pins WHERE pin_id=? AND calendar_id=? AND revision=? AND rule_id=? AND rule_version=? AND job_id=? AND job_attempt=? AND worker_id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var status string
	var expires time.Time
	err = tx.QueryRowContext(ctx, s.query(q), p.PinID, p.CalendarID, p.Revision, p.RuleID, p.RuleVersion, p.JobID, p.JobAttempt, p.WorkerID).Scan(&status, &expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (status != "active" || !expires.Equal(p.ExpiresAt)) {
		return ErrProjectionCheckpointConflict
	}
	if err != nil {
		return err
	}
	var sourceStart, sourceEnd time.Time
	err = tx.QueryRowContext(ctx, s.query(`SELECT window_start,window_end FROM projection_snapshots WHERE calendar_id=? AND revision=? AND verified=?`), p.CalendarID, p.Revision, true).Scan(&sourceStart, &sourceEnd)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (sourceStart.After(in.WindowStart) || sourceEnd.Before(in.WindowEnd)) {
		return ErrProjectionCheckpointConflict
	}
	if err != nil {
		return err
	}
	var currentVersion, currentRevision int64
	err = tx.QueryRowContext(ctx, s.query(`SELECT checkpoint_version,revision FROM projection_rule_checkpoints WHERE rule_id=?`), p.RuleID).Scan(&currentVersion, &currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		if in.ExpectedVersion != 0 {
			return ErrProjectionCheckpointConflict
		}
	} else if err != nil {
		return err
	} else if currentVersion != in.ExpectedVersion || p.Revision < currentRevision {
		return ErrProjectionCheckpointConflict
	}
	stamp := time.Now().UTC()
	if !expires.After(stamp) || !claimed.Add(p.LeaseDuration).After(stamp) {
		return ErrProjectionCheckpointConflict
	}
	if in.ExpectedVersion == 0 {
		_, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_rule_checkpoints(rule_id,checkpoint_version,calendar_id,revision,window_start,window_end,rule_version,job_id,job_attempt,worker_id,pin_id,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), p.RuleID, 1, p.CalendarID, p.Revision, in.WindowStart.UTC(), in.WindowEnd.UTC(), p.RuleVersion, p.JobID, p.JobAttempt, p.WorkerID, p.PinID, stamp)
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, s.query(`UPDATE projection_rule_checkpoints SET checkpoint_version=?,calendar_id=?,revision=?,window_start=?,window_end=?,rule_version=?,job_id=?,job_attempt=?,worker_id=?,pin_id=?,recorded_at=? WHERE rule_id=? AND checkpoint_version=?`), in.ExpectedVersion+1, p.CalendarID, p.Revision, in.WindowStart.UTC(), in.WindowEnd.UTC(), p.RuleVersion, p.JobID, p.JobAttempt, p.WorkerID, p.PinID, stamp, p.RuleID, in.ExpectedVersion)
		if err == nil {
			var n int64
			n, err = res.RowsAffected()
			if err == nil && n != 1 {
				return ErrProjectionCheckpointConflict
			}
		}
	}
	if err != nil {
		return fmt.Errorf("record projection checkpoint: %w", err)
	}
	stamp = time.Now().UTC()
	if !expires.After(stamp) || !claimed.Add(p.LeaseDuration).After(stamp) {
		return ErrProjectionCheckpointConflict
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit projection checkpoint: %w", err)
	}
	return nil
}

// GetProjectionCheckpoint returns diagnostic state, never write authorization.
func (s *Store) GetProjectionCheckpoint(ctx context.Context, ruleID string) (ProjectionCheckpoint, error) {
	var out ProjectionCheckpoint
	if ruleID == "" {
		return out, ErrProjectionCheckpointConflict
	}
	out.RuleID = ruleID
	err := s.db.QueryRowContext(ctx, s.query(`SELECT checkpoint_version,calendar_id,revision,window_start,window_end,rule_version,job_id,job_attempt,worker_id,pin_id,recorded_at FROM projection_rule_checkpoints WHERE rule_id=?`), ruleID).Scan(&out.CheckpointVersion, &out.CalendarID, &out.Revision, &out.WindowStart, &out.WindowEnd, &out.RuleVersion, &out.JobID, &out.JobAttempt, &out.WorkerID, &out.PinID, &out.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionCheckpoint{}, ErrNotFound
	}
	if err != nil {
		return ProjectionCheckpoint{}, err
	}
	return out, nil
}
