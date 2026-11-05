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
	var worker sql.NullString
	var attempt int
	var claimed, deadline sql.NullTime
	err := s.db.QueryRowContext(ctx, s.query(`SELECT state,claimed_by,attempt,claimed_at,lease_until FROM sync_jobs WHERE id=? AND rule_id=?`), job.ID, job.RuleID).Scan(&state, &worker, &attempt, &claimed, &deadline)
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
	if state != "running" || !worker.Valid || worker.String != job.ClaimedBy || attempt != job.Attempt || !claimed.Valid || claimed.Time.After(now) || !deadline.Valid || !deadline.Time.After(now) {
		return ErrJobLeaseLost
	}
	return nil
}
