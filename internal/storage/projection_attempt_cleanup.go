package storage

import (
	"context"
	"errors"
	"time"
)

var errProjectionAttemptNotObsolete = errors.New("projection attempt is not safely obsolete")

// discardObsoleteProjectionAttempt removes exactly one superseded staging
// attempt and its page rows. It is an isolated storage primitive, not a
// runtime GC: no current-generation attempt, snapshots, pins, checkpoints,
// provider data, or removal observation journal is removed. The strictly
// newer generation permanently fences every append/finalize of the old one.
func (s *Store) discardObsoleteProjectionAttempt(ctx context.Context, calendarID string, generation int64, attemptID string) error {
	if calendarID == "" || generation < 1 || !validProjectionMutationID(attemptID) {
		return errProjectionAttemptNotObsolete
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Follow staging lock order, including the source connection before lease.
	if err = lockProjectionPageSource(ctx, s, tx, calendarID); err != nil {
		return err
	}
	state, err := selectCalendarSyncStateForUpdate(ctx, s, tx, calendarID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if generation >= state.Generation || (state.LeaseUntil != nil && state.LeaseUntil.After(now)) || (state.LeaseOwner != "" && state.LeaseUntil == nil) {
		return errProjectionAttemptNotObsolete
	}
	res, err := tx.ExecContext(ctx, s.query(`DELETE FROM projection_page_attempts WHERE calendar_id=? AND generation=? AND attempt_id=?`), calendarID, generation, attemptID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errProjectionAttemptNotObsolete
	}
	return tx.Commit()
}
