package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *Store) ScheduleDueJobs(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	q := `SELECT id, interval_seconds FROM sync_rules WHERE state=? AND (next_run_at IS NULL OR next_run_at<=?) ORDER BY created_at`
	if s.dialect == DialectPostgres {
		q += " FOR UPDATE SKIP LOCKED"
	}
	rows, err := tx.QueryContext(ctx, s.query(q), "enabled", now)
	if err != nil {
		return 0, fmt.Errorf("select due rules: %w", err)
	}
	type dueRule struct {
		id       string
		interval int
	}
	var due []dueRule
	for rows.Next() {
		var value dueRule
		if err := rows.Scan(&value.id, &value.interval); err != nil {
			_ = rows.Close()
			return 0, err
		}
		due = append(due, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate due rules: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, rule := range due {
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO sync_jobs(id,rule_id,kind,state,available_at,attempt,created_at) VALUES (?,?,?,?,?,0,?)`), uuid.NewString(), rule.id, "scheduled", "pending", now, now); err != nil {
			return 0, fmt.Errorf("enqueue scheduled job: %w", err)
		}
		next := now.Add(time.Duration(rule.interval) * time.Second)
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE sync_rules SET next_run_at=?,updated_at=? WHERE id=?`), next, now, rule.id); err != nil {
			return 0, fmt.Errorf("advance rule schedule: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(due), nil
}

func (s *Store) EnqueueJob(ctx context.Context, j Job) error {
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO sync_jobs
		(id, rule_id, kind, state, available_at, claimed_at, claimed_by, attempt, created_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), j.ID, j.RuleID, j.Kind, j.State, j.AvailableAt,
		j.ClaimedAt, nullString(j.ClaimedBy), j.Attempt, j.CreatedAt, j.FinishedAt)
	if err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}
	return nil
}

func (s *Store) ClaimJob(ctx context.Context, workerID string, now time.Time) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := `SELECT candidate.id, candidate.rule_id, candidate.kind, candidate.state, candidate.available_at,
		candidate.claimed_at, candidate.claimed_by, candidate.attempt, candidate.created_at, candidate.finished_at
		FROM sync_jobs AS candidate WHERE candidate.state = ? AND candidate.available_at <= ?
		AND NOT EXISTS (SELECT 1 FROM sync_jobs AS active WHERE active.rule_id = candidate.rule_id AND active.state = ?)
		ORDER BY candidate.available_at, candidate.created_at LIMIT 1`
	if s.dialect == DialectPostgres {
		q += " FOR UPDATE SKIP LOCKED"
	}
	var j Job
	var claimed, finished sql.NullTime
	var claimedBy sql.NullString
	err = tx.QueryRowContext(ctx, s.query(q), "pending", now, "running").Scan(&j.ID, &j.RuleID, &j.Kind, &j.State,
		&j.AvailableAt, &claimed, &claimedBy, &j.Attempt, &j.CreatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select job: %w", err)
	}
	if s.dialect == DialectPostgres {
		if _, err := tx.ExecContext(ctx, s.query(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`), j.RuleID); err != nil {
			return nil, fmt.Errorf("lock job rule: %w", err)
		}
		var running int
		if err := tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM sync_jobs WHERE rule_id=? AND state=?`), j.RuleID, "running").Scan(&running); err != nil {
			return nil, fmt.Errorf("recheck running rule job: %w", err)
		}
		if running > 0 {
			return nil, nil
		}
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state = ?, claimed_at = ?, claimed_by = ?, attempt = attempt + 1
		WHERE id = ? AND state = ?`), "running", now, workerID, j.ID, "pending")
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	j.State, j.ClaimedAt, j.ClaimedBy, j.Attempt = "running", &now, workerID, j.Attempt+1
	return &j, nil
}

// RecoverStaleJobs expires abandoned worker claims. Any unfinished run is
// closed before the job is made eligible for a new attempt.
func (s *Store) RecoverStaleJobs(ctx context.Context, staleBefore, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE sync_runs SET outcome=?, finished_at=?, error_code=?, error_summary=?
		WHERE outcome=? AND job_id IN (SELECT id FROM sync_jobs WHERE state=? AND claimed_at<?)`),
		"failed", now, "worker_lease_expired", "Worker stopped before the synchronization run completed", "running", "running", staleBefore); err != nil {
		return 0, fmt.Errorf("close stale runs: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=?, available_at=?, claimed_at=NULL, claimed_by=NULL, finished_at=NULL
		WHERE state=? AND claimed_at<?`), "pending", now, "running", staleBefore)
	if err != nil {
		return 0, fmt.Errorf("recover stale jobs: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(count), nil
}

func (s *Store) RenewJobLease(ctx context.Context, job Job, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET claimed_at=?
		WHERE id=? AND state=? AND claimed_by=? AND attempt=?`), now, job.ID, "running", job.ClaimedBy, job.Attempt)
	if err != nil {
		return false, fmt.Errorf("renew job lease: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

func (s *Store) StartRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO sync_runs(id, job_id, rule_id, trigger_kind, outcome,
		started_at, finished_at, created_count, updated_count, deleted_count, skipped_count, warning_count,
		error_code, error_summary, dry_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.JobID, r.RuleID, r.Trigger, r.Outcome, r.StartedAt, r.FinishedAt, r.CreatedCount, r.UpdatedCount,
		r.DeletedCount, r.SkippedCount, r.WarningCount, nullString(r.ErrorCode), nullString(r.ErrorSummary), r.DryRun)
	if err != nil {
		return fmt.Errorf("start run: %w", err)
	}
	return nil
}

func (s *Store) FinishRun(ctx context.Context, runID string, job Job, outcome string, finished time.Time, counters Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=?, finished_at=?
		WHERE id=? AND state=? AND claimed_by=? AND attempt=?`), outcome, finished, job.ID, "running", job.ClaimedBy, job.Attempt)
	if err != nil {
		return fmt.Errorf("finish job: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrJobLeaseLost
	}
	res, err = tx.ExecContext(ctx, s.query(`UPDATE sync_runs SET outcome=?, finished_at=?, created_count=?, updated_count=?,
		deleted_count=?, skipped_count=?, warning_count=?, error_code=?, error_summary=? WHERE id=? AND job_id=? AND outcome=?`), outcome, finished,
		counters.CreatedCount, counters.UpdatedCount, counters.DeletedCount, counters.SkippedCount, counters.WarningCount,
		nullString(counters.ErrorCode), nullString(counters.ErrorSummary), runID, job.ID, "running")
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	changed, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("finish run %q: %w", runID, ErrNotFound)
	}
	return tx.Commit()
}

func (s *Store) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT id, job_id, rule_id, trigger_kind, outcome, started_at,
		finished_at, created_count, updated_count, deleted_count, skipped_count, warning_count, error_code,
		error_summary, dry_run FROM sync_runs ORDER BY started_at DESC LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var result []Run
	for rows.Next() {
		var r Run
		var finished sql.NullTime
		var code, summary sql.NullString
		if err := rows.Scan(&r.ID, &r.JobID, &r.RuleID, &r.Trigger, &r.Outcome, &r.StartedAt, &finished,
			&r.CreatedCount, &r.UpdatedCount, &r.DeletedCount, &r.SkippedCount, &r.WarningCount, &code, &summary, &r.DryRun); err != nil {
			return nil, err
		}
		if finished.Valid {
			r.FinishedAt = &finished.Time
		}
		r.ErrorCode, r.ErrorSummary = code.String, summary.String
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) HasSuccessfulDryRun(ctx context.Context, ruleID string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM sync_runs WHERE rule_id=? AND dry_run=? AND outcome=?`), ruleID, true, "succeeded").Scan(&count); err != nil {
		return false, fmt.Errorf("check successful dry run: %w", err)
	}
	return count > 0, nil
}

func (s *Store) UpsertMapping(ctx context.Context, m Mapping) error {
	q := `INSERT INTO event_mappings(id, rule_id, object_kind, source_event_id, source_series_id, original_start,
		target_event_id, target_series_id, content_hash, last_seen_at, reconciliation_state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule_id, source_event_id, original_start) DO UPDATE SET object_kind=excluded.object_kind,
		source_series_id=excluded.source_series_id, target_event_id=excluded.target_event_id,
		target_series_id=excluded.target_series_id, content_hash=excluded.content_hash,
		last_seen_at=excluded.last_seen_at, reconciliation_state=excluded.reconciliation_state`
	_, err := s.db.ExecContext(ctx, s.query(q), m.ID, m.RuleID, m.ObjectKind, m.SourceEventID, m.SourceSeriesID,
		m.OriginalStart, m.TargetEventID, m.TargetSeriesID, m.ContentHash, m.LastSeenAt, m.ReconciliationState)
	if err != nil {
		return fmt.Errorf("upsert mapping: %w", err)
	}
	return nil
}

func (s *Store) ListMappings(ctx context.Context, ruleID string) ([]Mapping, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT id, rule_id, object_kind, source_event_id, source_series_id,
		original_start, target_event_id, target_series_id, content_hash, last_seen_at, reconciliation_state
		FROM event_mappings WHERE rule_id=? ORDER BY source_event_id, original_start`), ruleID)
	if err != nil {
		return nil, fmt.Errorf("list mappings: %w", err)
	}
	defer rows.Close()
	var result []Mapping
	for rows.Next() {
		var m Mapping
		if err := rows.Scan(&m.ID, &m.RuleID, &m.ObjectKind, &m.SourceEventID, &m.SourceSeriesID, &m.OriginalStart,
			&m.TargetEventID, &m.TargetSeriesID, &m.ContentHash, &m.LastSeenAt, &m.ReconciliationState); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) DeleteMapping(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.query(`DELETE FROM event_mappings WHERE id=?`), id)
	if err != nil {
		return fmt.Errorf("delete mapping: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) PutOAuthAttempt(ctx context.Context, a OAuthAttempt) error {
	if a.Mode == "" {
		a.Mode = "add"
	}
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO oauth_attempts(state_hash, provider, connection_id, mode, encrypted_verifier, return_path, expires_at, consumed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), a.StateHash, a.Provider, nullString(a.ConnectionID), a.Mode, a.EncryptedVerifier, a.ReturnPath, a.ExpiresAt, a.ConsumedAt)
	if err != nil {
		return fmt.Errorf("put oauth attempt: %w", err)
	}
	return nil
}

func (s *Store) ConsumeOAuthAttempt(ctx context.Context, stateHash string, now time.Time) (OAuthAttempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var a OAuthAttempt
	var consumed sql.NullTime
	var connectionID sql.NullString
	err = tx.QueryRowContext(ctx, s.query(`SELECT state_hash, provider, connection_id, mode, encrypted_verifier, return_path, expires_at, consumed_at
		FROM oauth_attempts WHERE state_hash=?`), stateHash).Scan(&a.StateHash, &a.Provider, &connectionID, &a.Mode, &a.EncryptedVerifier, &a.ReturnPath, &a.ExpiresAt, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrOAuthNotConsumable
	}
	if err != nil {
		return a, fmt.Errorf("read oauth attempt: %w", err)
	}
	if consumed.Valid || !a.ExpiresAt.After(now) {
		return a, ErrOAuthNotConsumable
	}
	a.ConnectionID = connectionID.String
	res, err := tx.ExecContext(ctx, s.query(`UPDATE oauth_attempts SET consumed_at=? WHERE state_hash=? AND consumed_at IS NULL`), now, stateHash)
	if err != nil {
		return a, fmt.Errorf("consume oauth attempt: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return a, ErrOAuthNotConsumable
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	a.ConsumedAt = &now
	return a, nil
}

func (s *Store) DeleteExpiredOAuthAttempts(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.query(`DELETE FROM oauth_attempts WHERE expires_at <= ?`), now)
	if err != nil {
		return 0, fmt.Errorf("delete expired oauth attempts: %w", err)
	}
	return res.RowsAffected()
}
