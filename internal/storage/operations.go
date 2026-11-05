package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// JobLeaseDuration is the server-controlled job claim and heartbeat policy.
// A worker from an older binary must not run concurrently with schema 25:
// it does not maintain the persisted deadline.
const JobLeaseDuration = 15 * time.Minute

func (s *Store) ScheduleDueJobs(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	q := `SELECT id, interval_seconds FROM sync_rules WHERE state=? AND (next_run_at IS NULL OR next_run_at<=?)
		AND EXISTS (SELECT 1 FROM calendars source JOIN calendars target ON target.id=sync_rules.target_calendar_id
			WHERE source.id=sync_rules.source_calendar_id AND source.enabled=TRUE AND target.enabled=TRUE) ORDER BY created_at`
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
	result, err := s.db.ExecContext(ctx, s.query(`INSERT INTO sync_jobs
		(id, rule_id, kind, state, available_at, claimed_at, claimed_by, attempt, created_at, finished_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (
			SELECT 1 FROM sync_rules r JOIN calendars source ON source.id=r.source_calendar_id
			JOIN calendars target ON target.id=r.target_calendar_id
			WHERE r.id=? AND source.enabled=TRUE AND target.enabled=TRUE)`), j.ID, j.RuleID, j.Kind, j.State, j.AvailableAt,
		j.ClaimedAt, nullString(j.ClaimedBy), j.Attempt, j.CreatedAt, j.FinishedAt, j.RuleID)
	if err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return ErrCalendarSyncIneligible
	}
	return nil
}

func (s *Store) ClaimJob(ctx context.Context, workerID string, now time.Time) (*Job, error) {
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == DialectSQLite {
		// Acquire the writer lock before selecting or sampling the claim time.
		if _, err = tx.ExecContext(ctx, `UPDATE sync_jobs SET state=state WHERE 0`); err != nil {
			return nil, err
		}
	}
	q := `SELECT candidate.id, candidate.rule_id, candidate.kind, candidate.state, candidate.available_at,
		candidate.claimed_at, candidate.claimed_by, candidate.attempt, candidate.created_at, candidate.finished_at
		FROM sync_jobs AS candidate WHERE candidate.state = ? AND candidate.available_at <= ?
		AND EXISTS (SELECT 1 FROM sync_rules r JOIN calendars source ON source.id=r.source_calendar_id
			JOIN calendars target ON target.id=r.target_calendar_id
			WHERE r.id=candidate.rule_id AND source.enabled=TRUE AND target.enabled=TRUE)
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
	claimedAt := now.Add(time.Since(started))
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state = ?, claimed_at = ?, claimed_by = ?, lease_until = ?, attempt = attempt + 1
		WHERE id = ? AND state = ?`), "running", claimedAt, workerID, claimedAt.Add(JobLeaseDuration), j.ID, "pending")
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return nil, nil
	}
	if !claimedAt.Add(JobLeaseDuration).After(now.Add(time.Since(started))) {
		return nil, ErrJobLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	// A slow commit can consume the entire claim. Leave the committed job for
	// ordinary recovery; never return it as an executable claim after expiry.
	if !claimedAt.Add(JobLeaseDuration).After(now.Add(time.Since(started))) {
		return nil, ErrJobLeaseLost
	}
	j.State, j.ClaimedAt, j.ClaimedBy, j.Attempt = "running", &claimedAt, workerID, j.Attempt+1
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
	selectStale := `SELECT id FROM sync_jobs WHERE state=? AND claimed_at<? AND (lease_until IS NULL OR lease_until<=?) ORDER BY id`
	if s.dialect == DialectPostgres {
		selectStale += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, s.query(selectStale), "running", staleBefore, now)
	if err != nil {
		return 0, fmt.Errorf("select stale jobs: %w", err)
	}
	var candidates []string
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan stale job: %w", err)
		}
		candidates = append(candidates, jobID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate stale jobs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close stale jobs: %w", err)
	}
	recovered := make([]string, 0, len(candidates))
	for _, jobID := range candidates {
		res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=?, available_at=?, claimed_at=NULL, claimed_by=NULL, lease_until=NULL, finished_at=NULL
			WHERE id=? AND state=? AND claimed_at<? AND (lease_until IS NULL OR lease_until<=?)`), "pending", now, jobID, "running", staleBefore, now)
		if err != nil {
			return 0, fmt.Errorf("recover stale job %q: %w", jobID, err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed == 1 {
			recovered = append(recovered, jobID)
		}
	}
	for _, jobID := range recovered {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE sync_runs SET outcome=?, finished_at=?, error_code=?, error_summary=?
			WHERE outcome=? AND job_id=?`), "failed", now, "worker_lease_expired",
			"Worker stopped before the synchronization run completed", "running", jobID); err != nil {
			return 0, fmt.Errorf("close stale runs for job %q: %w", jobID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(recovered), nil
}

func (s *Store) RenewJobLease(ctx context.Context, job Job, now time.Time) (bool, error) {
	return s.renewJobLease(ctx, job, now, nil)
}

// beforeCommit is a private fault-injection seam for transaction timing tests.
func (s *Store) renewJobLease(ctx context.Context, job Job, now time.Time, beforeCommit func()) (bool, error) {
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	claimed, deadline, found, err := s.lockJobLeaseForUpdate(ctx, tx, job)
	if err != nil || !found {
		return false, err
	}
	lockedAt := now.Add(time.Since(started))
	if !claimed.Valid || claimed.Time.After(lockedAt) || deadline.Valid && !deadline.Time.After(lockedAt) {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET claimed_at=?,lease_until=CASE WHEN lease_until IS NULL THEN lease_until ELSE ? END
		WHERE id=? AND state=? AND claimed_by=? AND attempt=?`), lockedAt, lockedAt.Add(JobLeaseDuration), job.ID, "running", job.ClaimedBy, job.Attempt)
	if err != nil {
		return false, fmt.Errorf("renew job lease: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if count != 1 {
		return false, nil
	}
	if beforeCommit != nil {
		beforeCommit()
	}
	if deadline.Valid && !deadline.Time.After(now.Add(time.Since(started))) {
		return false, nil
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("commit job lease renewal: %w", err)
	}
	if deadline.Valid && !lockedAt.Add(JobLeaseDuration).After(now.Add(time.Since(started))) {
		return false, nil
	}
	return true, nil
}

// lockJobLeaseForUpdate must precede a fresh clock check. SQLite takes its
// write lock with a no-op update; PostgreSQL locks the matching job row.
func (s *Store) lockJobLeaseForUpdate(ctx context.Context, tx *sql.Tx, job Job) (sql.NullTime, sql.NullTime, bool, error) {
	var claimed, deadline sql.NullTime
	if s.dialect == DialectSQLite {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=state WHERE id=?`), job.ID); err != nil {
			return claimed, deadline, false, err
		}
	}
	q := `SELECT claimed_at,lease_until FROM sync_jobs WHERE id=? AND state=? AND claimed_by=? AND attempt=?`
	if s.dialect == DialectPostgres {
		q += ` FOR UPDATE`
	}
	err := tx.QueryRowContext(ctx, s.query(q), job.ID, "running", job.ClaimedBy, job.Attempt).Scan(&claimed, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return claimed, deadline, false, nil
	}
	if err != nil {
		return claimed, deadline, false, err
	}
	return claimed, deadline, true, nil
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
	return s.finishRun(ctx, runID, job, outcome, finished, counters, nil)
}

// beforeCommit is a private fault-injection seam for transaction timing tests.
func (s *Store) finishRun(ctx context.Context, runID string, job Job, outcome string, finished time.Time, counters Run, beforeCommit func()) error {
	started := time.Now()
	var encrypted []byte
	if counters.Preview != nil {
		if job.Kind != "dry_run" || outcome != "succeeded" || len(counters.Preview.Entries) > 1000 {
			return errors.New("invalid sync preview")
		}
		if s.artifactCipher == nil {
			return errors.New("sync preview encryption is unavailable")
		}
		data, err := json.Marshal(counters.Preview)
		if err != nil {
			return err
		}
		if len(data) > 4<<20 {
			return errors.New("sync preview exceeds size limit")
		}
		encrypted, err = s.artifactCipher.Encrypt(data, []byte("calendar-sync-preview\x00"+runID))
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, deadline, found, err := s.lockJobLeaseForUpdate(ctx, tx, job)
	if err != nil {
		return err
	}
	if !found || deadline.Valid && !deadline.Time.After(finished.Add(time.Since(started))) {
		return ErrJobLeaseLost
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=?, finished_at=?,lease_until=NULL
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
	if counters.Preview != nil && outcome == "succeeded" {
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO sync_run_previews (run_id, expires_at, payload) VALUES (?, ?, ?)`), runID, finished.Add(7*24*time.Hour), encrypted); err != nil {
			return fmt.Errorf("save sync preview: %w", err)
		}
	}
	if beforeCommit != nil {
		beforeCommit()
	}
	if deadline.Valid && !deadline.Time.After(finished.Add(time.Since(started))) {
		return ErrJobLeaseLost
	}
	return tx.Commit()
}

// GetSyncRunPreview returns a private preview and its availability state.
func (s *Store) GetSyncRunPreview(ctx context.Context, runID string, now time.Time) (*SyncRunPreview, string, error) {
	var expires time.Time
	var payload []byte
	err := s.db.QueryRowContext(ctx, s.query(`SELECT expires_at, payload FROM sync_run_previews WHERE run_id=?`), runID).Scan(&expires, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "unavailable", nil
	}
	if err != nil {
		return nil, "", err
	}
	if !now.Before(expires) || len(payload) == 0 {
		return nil, "expired", nil
	}
	if s.artifactCipher == nil {
		return nil, "", errors.New("sync preview encryption is unavailable")
	}
	data, err := s.artifactCipher.Decrypt(payload, []byte("calendar-sync-preview\x00"+runID))
	if err != nil {
		return nil, "", err
	}
	var preview SyncRunPreview
	if err := json.Unmarshal(data, &preview); err != nil {
		return nil, "", err
	}
	return &preview, "available", nil
}

// ExpireSyncRunPreviews removes encrypted private payloads after retention.
func (s *Store) ExpireSyncRunPreviews(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_run_previews SET payload=NULL WHERE payload IS NOT NULL AND expires_at<=?`), now)
	return err
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

// InsertPendingCreateMapping inserts an intent only when neither its identity
// nor target is already mapped. A false result never authorizes dispatch.
// This protects the mapping row, not a job lease or an in-flight HTTP request.
func (s *Store) InsertPendingCreateMapping(ctx context.Context, m Mapping) (string, error) {
	if m.ID == "" || m.RuleID == "" || m.SourceEventID == "" || m.ReconciliationState != "pending_create" || m.TargetEventID != "pending:"+m.ID || m.LastSeenAt.IsZero() {
		return "", errors.New("invalid pending creation intent")
	}
	token, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	incarnation := "new:" + token.String()
	res, err := s.db.ExecContext(ctx, s.query(`INSERT INTO event_mappings(id,rule_id,object_kind,source_event_id,source_series_id,original_start,target_event_id,target_series_id,content_hash,last_seen_at,reconciliation_state,incarnation) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`), m.ID, m.RuleID, m.ObjectKind, m.SourceEventID, m.SourceSeriesID, m.OriginalStart, m.TargetEventID, m.TargetSeriesID, m.ContentHash, m.LastSeenAt, m.ReconciliationState, incarnation)
	if err != nil {
		return "", fmt.Errorf("insert pending creation intent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return "", err
	}
	return incarnation, nil
}

func (s *Store) UpsertMapping(ctx context.Context, m Mapping) error {
	if strings.HasPrefix(m.ReconciliationState, "pending_") {
		return ErrMappingConflict
	}
	if m.Version > 0 {
		return s.updateMappingCAS(ctx, m, m.Version, true)
	}
	token, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	q := `INSERT INTO event_mappings(id, rule_id, object_kind, source_event_id, source_series_id, original_start,
		target_event_id, target_series_id, content_hash, last_seen_at, reconciliation_state,incarnation) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`
	res, err := s.db.ExecContext(ctx, s.query(q), m.ID, m.RuleID, m.ObjectKind, m.SourceEventID, m.SourceSeriesID,
		m.OriginalStart, m.TargetEventID, m.TargetSeriesID, m.ContentHash, m.LastSeenAt, m.ReconciliationState, "new:"+token.String())
	if err != nil {
		return fmt.Errorf("upsert mapping: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMappingConflict
	}
	return nil
}

func (s *Store) ListMappings(ctx context.Context, ruleID string) ([]Mapping, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT id, rule_id, object_kind, source_event_id, source_series_id,
		original_start, target_event_id, target_series_id, content_hash, last_seen_at, reconciliation_state, version,incarnation,target_etag
		FROM event_mappings WHERE rule_id=? ORDER BY source_event_id, original_start`), ruleID)
	if err != nil {
		return nil, fmt.Errorf("list mappings: %w", err)
	}
	defer rows.Close()
	var result []Mapping
	for rows.Next() {
		var m Mapping
		if err := rows.Scan(&m.ID, &m.RuleID, &m.ObjectKind, &m.SourceEventID, &m.SourceSeriesID, &m.OriginalStart,
			&m.TargetEventID, &m.TargetSeriesID, &m.ContentHash, &m.LastSeenAt, &m.ReconciliationState, &m.Version, &m.Incarnation, &m.TargetETag); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) DeleteMapping(ctx context.Context, id string) error {
	// Deleting without an incarnation can remove a later replacement row.
	return ErrMappingConflict
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
