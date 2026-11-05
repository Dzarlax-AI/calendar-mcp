package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"calendar-mcp/internal/calendar"
)

var ErrProjectionFinalizerRequired = errors.New("atomic projection finalizer is required")

// ApplyProjectionStagedPage explicitly opts into an experimental transaction
// that stages a canonical Google page and applies that same page to the legacy
// projection. It does not publish an immutable revision or authorize any
// target write, and is not called by the live sync coordinator. Caller-supplied
// now is a trusted clock sample; monotonic elapsed time includes lock waits.
// Already-staged pages fail closed: their presence does not prove the legacy
// application was part of the same transaction.
func (s *Store) ApplyProjectionStagedPage(ctx context.Context, state CalendarSyncState, attemptID string, ordinal int, previousToken, observedTimezone string, page calendar.EventSyncPage, now time.Time) error {
	if page.Complete {
		return ErrProjectionFinalizerRequired
	}
	if state.CalendarID == "" || state.LeaseOwner == "" || now.IsZero() {
		return ErrProjectionPageStage
	}
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin staged page apply: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockProjectionPageSource(ctx, s, tx, state.CalendarID); err != nil {
		return err
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	if err = s.appendProjectionPageTx(ctx, tx, state, attemptID, ordinal, previousToken, observedTimezone, page, now, started); err != nil {
		return err
	}
	var mode string
	var leaseUntil time.Time
	err = tx.QueryRowContext(ctx, s.query(`SELECT p.mode,st.lease_until FROM projection_page_attempts p JOIN calendar_sync_state st ON st.calendar_id=p.calendar_id AND st.generation=p.generation WHERE p.calendar_id=? AND p.generation=? AND p.attempt_id=?`), state.CalendarID, state.Generation, attemptID).Scan(&mode, &leaseUntil)
	if err != nil {
		return fmt.Errorf("read staged page claim: %w", err)
	}
	if mode != string(calendar.EventSyncReplacement) && mode != string(calendar.EventSyncIncremental) {
		return ErrProjectionPageStage
	}
	batch := EventSyncBatch{FullSync: mode == string(calendar.EventSyncReplacement), DeletedEventIDs: append([]string(nil), page.DeletedEventIDs...)}

	for _, upsert := range page.Upserts {
		batch.Upserts = append(batch.Upserts, CachedEventUpsert{SourceObjectID: upsert.Object.ObjectID, Event: upsert.Event})
		batch.Objects = append(batch.Objects, SyncObject{ObjectID: upsert.Object.ObjectID, ETag: upsert.Object.ETag})
	}
	if err = s.applyEventSyncPageTx(ctx, tx, state, batch, false, now.Add(time.Since(started))); err != nil {
		return err
	}
	if !now.Add(time.Since(started)).Before(leaseUntil) {
		return ErrCalendarSyncLeaseLost
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit staged page apply: %w", err)
	}
	return nil
}
