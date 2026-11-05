package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// acquireSourceStateFenceTx serializes source state transitions with cache
// Runs without holding a reservation across provider reads. The temporary row
// and its ledger are removed in the same transaction, before commit.
func (s *Store) acquireSourceStateFenceTx(ctx context.Context, tx *sql.Tx, now time.Time) (string, bool, error) {
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operations(operation_id,calendar_ids,started_at) VALUES(?,?,?)`), id, "[]", now.UTC()); err != nil {
		return "", false, err
	}
	err := s.insertExclusiveReservation(ctx, tx, ExclusiveDispatchRequest{OperationID: id, TargetKey: GlobalSyncDispatchKey, RequestHash: projectionPageHash([]byte(id)), Kind: "run", Now: now})
	if errors.Is(err, ErrExclusiveDispatchConflict) {
		if _, e := tx.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operations WHERE operation_id=?`), id); e != nil {
			return "", false, e
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}
func (s *Store) releaseSourceStateFenceTx(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM exclusive_dispatch_reservations WHERE operation_id=?`), id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operations WHERE operation_id=?`), id)
	return err
}

// DeferCalendarSyncForReservation releases only the current source lease while
// preserving cursor, window and last success. It restores ready solely when an
// existing verified snapshot still matches the current clean source state.
func (s *Store) DeferCalendarSyncForReservation(ctx context.Context, state CalendarSyncState, now, next time.Time) error {
	if state.CalendarID == "" || state.LeaseOwner == "" || now.IsZero() || !next.After(now) {
		return ErrCalendarSyncLeaseLost
	}
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	var ready, quarantine bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_mutation_guards g JOIN projection_snapshots p ON p.calendar_id=g.calendar_id AND p.revision=g.revision JOIN projection_revision_provenance v ON v.calendar_id=p.calendar_id AND v.revision=p.revision JOIN calendar_sync_state st ON st.calendar_id=g.calendar_id JOIN calendars c ON c.id=g.calendar_id JOIN connections conn ON conn.id=c.connection_id WHERE g.calendar_id=? AND p.verified=? AND g.epoch=p.guard_epoch AND st.cursor=p.cursor AND st.cursor<>'' AND st.window_start=p.window_start AND st.window_end=p.window_end AND c.timezone=p.timezone AND c.enabled=? AND c.can_read=? AND conn.status=? AND conn.provider=? AND st.last_success_at IS NOT NULL AND st.last_success_at>=p.completed_at AND NOT EXISTS(SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=g.calendar_id)),EXISTS(SELECT 1 FROM calendar_sync_quarantine q WHERE q.calendar_id=? AND q.active=?)`), state.CalendarID, true, true, true, "connected", "google", state.CalendarID, true).Scan(&ready, &quarantine)
	if err != nil {
		return err
	}
	status := "pending"
	if quarantine {
		status = "degraded"
	} else if ready {
		status = "ready"
	}
	checked := now.Add(time.Since(started))
	if !next.After(checked) {
		return ErrCalendarSyncLeaseLost
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET status=?,next_sync_at=?,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`), status, next.UTC(), checked.UTC(), state.CalendarID, state.Generation, "syncing", state.LeaseOwner, checked)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCalendarSyncLeaseLost
	}
	return tx.Commit()
}
