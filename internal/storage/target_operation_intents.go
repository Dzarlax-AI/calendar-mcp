package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

var ErrTargetIntentBlocked = errors.New("target operation intent unavailable or already recorded")

const maxTargetIntentsPerRule = 2048
const maxTargetIntentLease = 15 * time.Minute

// TargetIntentRequest binds an operation to a source snapshot and a rule/job
// claim. Hashes are SHA-256 hex, and the marker/ETag are untrusted assertions:
// provider ownership and conditional-write support must still be verified.
// A successful insert never itself authorizes a provider call. The current
// snapshot publication is not a verified completeness, source-status or
// quarantine contract. A destination may itself be another rule source; this
// method does not atomically dirty/fence its source projection. This API is
// diagnostic only until both contracts are integrated.
type TargetIntentRequest struct {
	OperationID, RuleID, JobID, WorkerID         string
	JobAttempt                                   int
	RuleVersion                                  int64
	SourceCalendarID, TargetCalendarID           string
	SourceRevision                               int64
	WindowStart, WindowEnd                       time.Time
	IdentityHash, PayloadHash, Kind              string
	TargetEventID, ExpectedETag, OwnershipMarker string
	Now                                          time.Time
	LeaseDuration, SnapshotMaxAge                time.Duration
}

type TargetIntentStatus struct{ OperationID, Status string }

func targetDigest(s string) bool {
	v, err := hex.DecodeString(s)
	return err == nil && len(v) == 32 && hex.EncodeToString(v) == s
}
func boundedTargetText(s string, max int) bool { return len(s) <= max }

// PrepareTargetIntent persists exactly one operation ID, and at most one
// unresolved operation per rule and logical occurrence. Every existing ID,
// including an ambiguous one, is rejected on replay. A crash after commit is
// ambiguous: callers must park the intent instead of reissuing the operation.
// This is an isolated storage primitive, not connected to the current engine.
func (s *Store) PrepareTargetIntent(ctx context.Context, in TargetIntentRequest) error {
	if !validProjectionMutationID(in.OperationID) || in.RuleID == "" || in.JobID == "" || in.WorkerID == "" || in.SourceCalendarID == "" || in.TargetCalendarID == "" || in.SourceCalendarID == in.TargetCalendarID || in.JobAttempt < 1 || in.RuleVersion < 1 || in.SourceRevision < 1 || in.Now.IsZero() || in.WindowStart.IsZero() || !in.WindowEnd.After(in.WindowStart) || in.LeaseDuration <= 0 || in.LeaseDuration > maxTargetIntentLease || in.SnapshotMaxAge <= 0 || !targetDigest(in.IdentityHash) || !targetDigest(in.PayloadHash) || !boundedTargetText(in.RuleID, 512) || !boundedTargetText(in.JobID, 512) || !boundedTargetText(in.WorkerID, 512) || !boundedTargetText(in.SourceCalendarID, 1024) || !boundedTargetText(in.TargetCalendarID, 1024) || !boundedTargetText(in.TargetEventID, 1024) || !boundedTargetText(in.ExpectedETag, 1024) || in.OwnershipMarker == "" || !boundedTargetText(in.OwnershipMarker, 1024) {
		return ErrTargetIntentBlocked
	}
	switch in.Kind {
	case "create":
		if in.TargetEventID != "" || in.ExpectedETag != "" {
			return ErrTargetIntentBlocked
		}
	case "update", "delete":
		if in.TargetEventID == "" || in.ExpectedETag == "" {
			return ErrTargetIntentBlocked
		}
	default:
		return ErrTargetIntentBlocked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ids := []string{in.SourceCalendarID, in.TargetCalendarID}
	sort.Strings(ids)
	for _, id := range ids {
		if s.dialect == DialectSQLite {
			if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET enabled=enabled WHERE id=?`), id); err != nil {
				return fmt.Errorf("lock target operation calendar: %w", err)
			}
		}
		q := `SELECT enabled,can_read,can_write FROM calendars WHERE id=?`
		if s.dialect == DialectPostgres {
			q += ` FOR NO KEY UPDATE`
		}
		var enabled, read, write bool
		err = tx.QueryRowContext(ctx, s.query(q), id).Scan(&enabled, &read, &write)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (!enabled || id == in.SourceCalendarID && !read || id == in.TargetCalendarID && !write) {
			return ErrTargetIntentBlocked
		}
		if err != nil {
			return err
		}
	}
	if err = lockProjectionGuardRow(ctx, s, tx, in.SourceCalendarID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrTargetIntentBlocked
		}
		return err
	}
	var epoch, revision, snapshotEpoch, pending int64
	var verified bool
	var start, end, completed time.Time
	err = tx.QueryRowContext(ctx, s.query(`SELECT g.epoch,g.revision,p.guard_epoch,p.window_start,p.window_end,p.completed_at,p.verified,(SELECT COUNT(*) FROM projection_mutation_intents i WHERE i.calendar_id=g.calendar_id) FROM projection_mutation_guards g JOIN projection_snapshots p ON p.calendar_id=g.calendar_id AND p.revision=? WHERE g.calendar_id=?`), in.SourceRevision, in.SourceCalendarID).Scan(&epoch, &revision, &snapshotEpoch, &start, &end, &completed, &verified, &pending)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (!verified || revision != in.SourceRevision || epoch != snapshotEpoch || pending != 0 || start.After(in.WindowStart) || end.Before(in.WindowEnd) || completed.After(in.Now) || completed.Before(in.Now.Add(-in.SnapshotMaxAge))) {
		return ErrTargetIntentBlocked
	}
	if err != nil {
		return err
	}
	q := `SELECT state,config_version,source_calendar_id,target_calendar_id FROM sync_rules WHERE id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var state, source, target string
	var version int64
	err = tx.QueryRowContext(ctx, s.query(q), in.RuleID).Scan(&state, &version, &source, &target)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (state != "enabled" || version != in.RuleVersion || source != in.SourceCalendarID || target != in.TargetCalendarID) {
		return ErrTargetIntentBlocked
	}
	if err != nil {
		return err
	}
	q = `SELECT state,claimed_by,attempt,claimed_at FROM sync_jobs WHERE id=? AND rule_id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var jobState string
	var owner sql.NullString
	var attempt int
	var claimed sql.NullTime
	err = tx.QueryRowContext(ctx, s.query(q), in.JobID, in.RuleID).Scan(&jobState, &owner, &attempt, &claimed)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (jobState != "running" || !owner.Valid || owner.String != in.WorkerID || attempt != in.JobAttempt || !claimed.Valid || claimed.Time.Before(in.Now.Add(-in.LeaseDuration)) || claimed.Time.After(in.Now)) {
		return ErrTargetIntentBlocked
	}
	if err != nil {
		return err
	}
	// Recheck against a fresh clock after all potentially blocking row locks.
	// The caller's request timestamp can have aged while waiting for the job.
	lockedAt := time.Now().UTC()
	if claimed.Time.Before(lockedAt.Add(-in.LeaseDuration)) || claimed.Time.After(lockedAt) || completed.Before(lockedAt.Add(-in.SnapshotMaxAge)) || completed.After(lockedAt) {
		return ErrTargetIntentBlocked
	}
	var count int
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM target_operation_intents WHERE rule_id=?`), in.RuleID).Scan(&count); err != nil {
		return err
	}
	if count >= maxTargetIntentsPerRule {
		return ErrTargetIntentBlocked
	}
	// Explicit lookups make ordinary duplicate/replay a stable error across dialects.
	var exists bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM target_operation_intents WHERE operation_id=? OR (rule_id=? AND identity_hash=? AND status IN ('pending','ambiguous')))`), in.OperationID, in.RuleID, in.IdentityHash).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrTargetIntentBlocked
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO target_operation_intents(operation_id,rule_id,source_calendar_id,target_calendar_id,source_revision,window_start,window_end,identity_hash,target_event_id,expected_etag,ownership_marker,payload_hash,operation_kind,rule_version,job_id,job_attempt,worker_id,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), in.OperationID, in.RuleID, in.SourceCalendarID, in.TargetCalendarID, in.SourceRevision, in.WindowStart.UTC(), in.WindowEnd.UTC(), in.IdentityHash, in.TargetEventID, in.ExpectedETag, in.OwnershipMarker, in.PayloadHash, in.Kind, in.RuleVersion, in.JobID, in.JobAttempt, in.WorkerID, "pending", in.Now.UTC())
	if err != nil {
		return fmt.Errorf("insert target intent: %w", err)
	}
	// Count, uniqueness enforcement and the insert can wait on locks too.
	// Refuse to commit if the lease or source age expired during that wait.
	commitAt := time.Now().UTC()
	if claimed.Time.Before(commitAt.Add(-in.LeaseDuration)) || claimed.Time.After(commitAt) || completed.Before(commitAt.Add(-in.SnapshotMaxAge)) || completed.After(commitAt) {
		return ErrTargetIntentBlocked
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit target intent: %w", err)
	}
	return nil
}

// MarkTargetIntentAmbiguous is terminal until a future explicit reconciliation
// protocol supplies proof. It never grants permission to repeat a provider call.
func (s *Store) MarkTargetIntentAmbiguous(ctx context.Context, operationID string, now time.Time) error {
	if !validProjectionMutationID(operationID) || now.IsZero() {
		return ErrTargetIntentBlocked
	}
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE target_operation_intents SET status='ambiguous',completed_at=? WHERE operation_id=? AND status='pending'`), now.UTC(), operationID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTargetIntentBlocked
	}
	return nil
}

func (s *Store) GetTargetIntentStatus(ctx context.Context, operationID string) (TargetIntentStatus, error) {
	var out TargetIntentStatus
	if !validProjectionMutationID(operationID) {
		return out, ErrTargetIntentBlocked
	}
	out.OperationID = operationID
	err := s.db.QueryRowContext(ctx, s.query(`SELECT status FROM target_operation_intents WHERE operation_id=?`), operationID).Scan(&out.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return TargetIntentStatus{}, ErrNotFound
	}
	return out, err
}
