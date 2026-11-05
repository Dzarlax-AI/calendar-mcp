package storage

import (
	"context"
	"fmt"
	"time"

	"calendar-mcp/internal/calendar"
	"github.com/google/uuid"
)

// finalizeSourceProjectionPage stages the terminal Google page and publishes
// the isolated source snapshot with its cursor in one transaction. The
// snapshot is constructed from staged provider pages, never from legacy cache
// rows. This function remains private until event-sync owns the whole source
// staging lifecycle and retention can preserve outstanding cancellations.
func (s *Store) finalizeSourceProjectionPage(ctx context.Context, state CalendarSyncState, attemptID string, ordinal int, previousToken, timezone string, page calendar.EventSyncPage, now, nextSync time.Time) (int64, error) {
	if !page.Complete || now.IsZero() {
		return 0, ErrProjectionPageStage
	}
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// Reserve the same global key as every cache/direct sync Run before taking
	// source locks. A competing uncommitted acquisition is serialized by the
	// unique index; an active/parked Run prevents publication, not reads.
	operationID := uuid.NewString()
	_, encoded, err := normalizeMutationCalendars([]string{state.CalendarID})
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operations(operation_id,calendar_ids,started_at) VALUES(?,?,?)`), operationID, encoded, now.UTC()); err != nil {
		return 0, err
	}
	reservation := ExclusiveDispatchRequest{OperationID: operationID, TargetKey: GlobalSyncDispatchKey, RequestHash: projectionPageHash([]byte(operationID)), Kind: "run", Now: now}
	if err = s.insertExclusiveReservation(ctx, tx, reservation); err != nil {
		return 0, err
	}
	if err = lockProjectionPageSource(ctx, s, tx, state.CalendarID); err != nil {
		return 0, err
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return 0, err
	}
	var leaseUntil time.Time
	if err = tx.QueryRowContext(ctx, s.query(`SELECT lease_until FROM calendar_sync_state WHERE calendar_id=? AND generation=? AND lease_owner=? AND status=?`), state.CalendarID, state.Generation, state.LeaseOwner, "syncing").Scan(&leaseUntil); err != nil {
		return 0, ErrCalendarSyncLeaseLost
	}
	if err = s.appendProjectionPageTx(ctx, tx, state, attemptID, ordinal, previousToken, timezone, page, now, started); err != nil {
		return 0, err
	}
	revision, err := s.finalizeProjectionTx(ctx, tx, state, attemptID, now.Add(time.Since(started)), nextSync)
	if err != nil {
		return 0, err
	}
	// Remove only our transaction-local reservation. No provider IO occurs
	// under it, and rollback removes both rows on every failure. Concurrent
	// acquisitions remain serialized until this transaction commits.
	if _, err = tx.ExecContext(ctx, s.query(`DELETE FROM exclusive_dispatch_reservations WHERE operation_id=?`), operationID); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operations WHERE operation_id=?`), operationID); err != nil {
		return 0, err
	}
	if !now.Add(time.Since(started)).Before(leaseUntil) || !nextSync.After(now.Add(time.Since(started))) {
		return 0, ErrCalendarSyncLeaseLost
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit source projection: %w", err)
	}
	return revision, nil
}
