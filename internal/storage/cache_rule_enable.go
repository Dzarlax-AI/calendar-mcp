package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// EnableCachedRuleIfReady checks the current verified source and destination
// eligibility under the rule graph and source locks. Provider ownership is
// rechecked again under the global run fence before every write.
func (s *Store) EnableCachedRuleIfReady(ctx context.Context, id string, now time.Time, maxAge time.Duration) error {
	if id == "" || now.IsZero() || maxAge <= 0 {
		return ErrCacheExecutionIneligible
	}
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockRuleGraph(ctx, tx); err != nil {
		return err
	}
	var source, target, mode, state string
	var lookback, lookahead int
	var version int64
	q := `SELECT source_calendar_id,target_calendar_id,sync_mode,state,lookback_days,lookahead_days,config_version FROM sync_rules WHERE id=?`
	if err = tx.QueryRowContext(ctx, s.query(q), id).Scan(&source, &target, &mode, &state, &lookback, &lookahead, &version); errors.Is(err, sql.ErrNoRows) {
		return ErrCacheExecutionIneligible
	} else if err != nil {
		return err
	}
	if mode != "cached_instances" || state != "paused" {
		return ErrCacheExecutionIneligible
	}
	ids, _, err := normalizeMutationCalendars([]string{source, target})
	if err != nil || len(ids) != 2 {
		return ErrCacheExecutionIneligible
	}
	if err = lockMutationCalendars(ctx, s, tx, ids); err != nil {
		return err
	}
	if err = lockProjectionPageSource(ctx, s, tx, source); err != nil {
		return err
	}
	if _, err = selectCalendarSyncStateForUpdate(ctx, s, tx, source); err != nil {
		return ErrProjectionSnapshotUnavailable
	}
	if err = lockProjectionGuardRow(ctx, s, tx, source); err != nil {
		return ErrProjectionSnapshotUnavailable
	}
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var lockedSource, lockedTarget, lockedMode, lockedState string
	var lockedLookback, lockedLookahead int
	var lockedVersion int64
	if err = tx.QueryRowContext(ctx, s.query(q), id).Scan(&lockedSource, &lockedTarget, &lockedMode, &lockedState, &lockedLookback, &lockedLookahead, &lockedVersion); err != nil {
		return err
	}
	if lockedSource != source || lockedTarget != target || lockedMode != mode || lockedState != state || lockedLookback != lookback || lockedLookahead != lookahead || lockedVersion != version {
		return ErrCacheExecutionIneligible
	}
	if err = s.checkSyncTargetEligibility(ctx, tx, id); err != nil {
		return err
	}
	checked := now.Add(time.Since(started))
	start, end := checked.AddDate(0, 0, -lookback), checked.AddDate(0, 0, lookahead)
	projection, err := s.getSourceProjectionForCopy(ctx, tx, source, start, end, checked, maxAge)
	if err != nil {
		return err
	}
	// Only the latest unexpired source preview can authorize the transition.
	// Its version was captured on both sides of the worker's rule read.
	var runID string
	var expires time.Time
	var ciphertext []byte
	err = tx.QueryRowContext(ctx, s.query(`SELECT r.id,p.expires_at,p.payload FROM sync_runs r JOIN sync_run_previews p ON p.run_id=r.id WHERE r.rule_id=? AND r.dry_run=? AND r.outcome=? ORDER BY r.finished_at DESC,r.id DESC LIMIT 1`), id, true, "succeeded").Scan(&runID, &expires, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCacheExecutionIneligible
	}
	if err != nil {
		return err
	}
	if s.artifactCipher == nil || !checked.Before(expires) || len(ciphertext) == 0 {
		return ErrCacheExecutionIneligible
	}
	plain, err := s.artifactCipher.Decrypt(ciphertext, []byte("calendar-sync-preview\x00"+runID))
	if err != nil {
		return ErrCacheExecutionIneligible
	}
	var preview SyncRunPreview
	if err = json.Unmarshal(plain, &preview); err != nil || preview.RuleConfigVersion != version || preview.SourceRevision != projection.Snapshot.Revision || preview.TargetStatus != "not_checked" {
		return ErrCacheExecutionIneligible
	}
	checked = now.Add(time.Since(started))
	if _, err = s.getSourceProjectionForCopy(ctx, tx, source, checked.AddDate(0, 0, -lookback), checked.AddDate(0, 0, lookahead), checked, maxAge); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_rules SET state='enabled',next_run_at=?,updated_at=? WHERE id=? AND sync_mode='cached_instances' AND state='paused' AND config_version=? AND NOT EXISTS (SELECT 1 FROM sync_jobs WHERE rule_id=sync_rules.id AND (state='unknown' OR (state='running' AND stop_requested=TRUE))) AND NOT EXISTS (SELECT 1 FROM exclusive_dispatch_reservations WHERE released_at IS NULL)`), checked, checked, id, version)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCacheExecutionIneligible
	}
	return tx.Commit()
}
