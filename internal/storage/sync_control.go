package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrSyncControlConflict = errors.New("sync control cannot be completed safely")

// RequestRulePause stops future scheduling and requests stop of current work.
// A committed request does not prove that an in-flight provider call stopped.
func (s *Store) RequestRulePause(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_rules SET state='paused', next_run_at=NULL, updated_at=? WHERE id=? AND state='enabled'`), now, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSyncControlConflict
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state='cancelled', finished_at=? WHERE rule_id=? AND state='pending' AND kind<>'dry_run'`), now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET stop_requested=TRUE WHERE rule_id=? AND state='running' AND kind<>'dry_run'`), id); err != nil {
		return err
	}
	return tx.Commit()
}

// RequestJobStop cancels a queued job or requests a running worker to stop.
// It never changes provider state, clears evidence, or claims an in-flight call
// has ended. Duplicate or stale requests fail closed.
func (s *Store) RequestJobStop(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=CASE WHEN state='pending' THEN 'cancelled' ELSE state END, finished_at=CASE WHEN state='pending' THEN ? ELSE finished_at END, stop_requested=TRUE WHERE id=? AND state IN ('pending','running') AND stop_requested=FALSE`), now, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSyncControlConflict
	}
	return nil
}

func (s *Store) JobStopRequested(ctx context.Context, job Job) (bool, error) {
	var stop bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT stop_requested FROM sync_jobs WHERE id=? AND rule_id=? AND state='running' AND claimed_by=? AND attempt=?`), job.ID, job.RuleID, job.ClaimedBy, job.Attempt).Scan(&stop)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrJobLeaseLost
	}
	if err != nil {
		return false, fmt.Errorf("read job stop request: %w", err)
	}
	return stop, nil
}

// ArchiveRule removes a paused rule from active management while retaining its
// mappings and history. The global reservation is intentionally conservative:
// it cannot currently be attributed reliably to a single historical job.
func (s *Store) ArchiveRule(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.lockRuleGraph(ctx, tx); err != nil {
		return err
	}
	if s.dialect == DialectSQLite {
		if _, err = tx.ExecContext(ctx, s.query(`UPDATE sync_rules SET state=state WHERE id=?`), id); err != nil {
			return err
		}
	}
	q := `SELECT state FROM sync_rules WHERE id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR UPDATE`
	}
	var state string
	if err = tx.QueryRowContext(ctx, s.query(q), id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return ErrSyncControlConflict
	} else if err != nil {
		return err
	}
	if state != "paused" {
		return ErrSyncControlConflict
	}
	var blocked bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM sync_jobs WHERE rule_id=? AND state IN ('pending','running','unknown')) OR EXISTS(SELECT 1 FROM event_mappings WHERE rule_id=? AND reconciliation_state IN ('pending_create','pending_update','pending_delete')) OR EXISTS(SELECT 1 FROM target_operation_intents WHERE rule_id=? AND status IN ('pending','ambiguous')) OR EXISTS(SELECT 1 FROM exclusive_dispatch_reservations WHERE released_at IS NULL)`), id, id, id).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return ErrSyncControlConflict
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_rules SET state='archived',next_run_at=NULL,updated_at=? WHERE id=? AND state='paused'`), now, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSyncControlConflict
	}
	return tx.Commit()
}

// RuleControlStatus reports a conservative snapshot for a paused rule. An
// outstanding global reservation cannot be attributed safely to one rule.
func (s *Store) RuleControlStatus(ctx context.Context, ruleID string) (string, error) {
	var state string
	var active, stopping, dryRun, uncertain bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT r.state,
		EXISTS(SELECT 1 FROM sync_jobs WHERE rule_id=r.id AND state IN ('pending','running') AND kind<>'dry_run'),
		EXISTS(SELECT 1 FROM sync_jobs WHERE rule_id=r.id AND state='running' AND stop_requested=TRUE),
		EXISTS(SELECT 1 FROM sync_jobs WHERE rule_id=r.id AND state IN ('pending','running') AND kind='dry_run'),
		EXISTS(SELECT 1 FROM sync_jobs WHERE rule_id=r.id AND state='unknown') OR EXISTS(SELECT 1 FROM event_mappings WHERE rule_id=r.id AND reconciliation_state IN ('pending_create','pending_update','pending_delete')) OR EXISTS(SELECT 1 FROM target_operation_intents WHERE rule_id=r.id AND status IN ('pending','ambiguous')) OR EXISTS(SELECT 1 FROM exclusive_dispatch_reservations WHERE released_at IS NULL)
		FROM sync_rules r WHERE r.id=?`), ruleID).Scan(&state, &active, &stopping, &dryRun, &uncertain)
	if err != nil {
		return "", err
	}
	if state == "enabled" {
		if stopping {
			return "stopping", nil
		}
		if !active && uncertain {
			return "needs_review", nil
		}
		return "active", nil
	}
	if state != "paused" {
		return "unknown", nil
	}
	if active {
		return "pausing", nil
	}
	if uncertain {
		return "needs_review", nil
	}
	if dryRun {
		return "busy", nil
	}
	return "paused", nil
}

func (s *Store) ListPendingSyncJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,rule_id,kind,created_at FROM sync_jobs WHERE state='pending' ORDER BY created_at LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		var j Job
		if err = rows.Scan(&j.ID, &j.RuleID, &j.Kind, &j.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
