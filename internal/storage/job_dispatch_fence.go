package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CheckJobDispatchLease checks current durable state immediately before a
// provider call. A result is valid only at the instant of this observation:
// reclaim, expiry and a delayed HTTP request can race after return. It neither
// reserves a target nor permits retry of an unknown provider outcome.
func (s *Store) CheckJobDispatchLease(ctx context.Context, job Job) error {
	if s == nil || job.ID == "" || job.RuleID == "" || job.ClaimedBy == "" || job.Attempt < 1 {
		return ErrJobLeaseLost
	}
	var state string
	var stopRequested bool
	var ruleState, kind string
	var worker sql.NullString
	var attempt int
	var claimed, deadline sql.NullTime
	err := s.db.QueryRowContext(ctx, s.query(`SELECT j.state,j.claimed_by,j.attempt,j.claimed_at,j.lease_until,j.stop_requested,r.state,j.kind FROM sync_jobs j JOIN sync_rules r ON r.id=j.rule_id WHERE j.id=? AND j.rule_id=?`), job.ID, job.RuleID).Scan(&state, &worker, &attempt, &claimed, &deadline, &stopRequested, &ruleState, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrJobLeaseLost
	}
	if err != nil {
		return fmt.Errorf("check job dispatch lease: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if stopRequested || (kind != "dry_run" && ruleState != "enabled") || state != "running" || !worker.Valid || worker.String != job.ClaimedBy || attempt != job.Attempt || !claimed.Valid || claimed.Time.After(now) || !deadline.Valid || !deadline.Time.After(now) {
		return ErrJobLeaseLost
	}
	return nil
}
