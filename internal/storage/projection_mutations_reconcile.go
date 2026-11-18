package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResolveProjectionMutationManyAndSchedule atomically removes all intents and
// invalidates the source cursor/lease for a new replacement attempt. The caller
// must have confirmed the provider result; this function does not do so. A
// scheduled replacement is not itself a completed or copy-safe snapshot.
func (s *Store) ResolveProjectionMutationManyAndSchedule(ctx context.Context, calendarIDs []string, operationID string, now time.Time) error {
	ids, encoded, err := normalizeMutationCalendars(calendarIDs)
	if err != nil || !validProjectionMutationID(operationID) || now.IsZero() {
		return ErrProjectionMutationManyConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == DialectSQLite {
		if _, err = tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_operations SET operation_id=operation_id WHERE operation_id=?`), operationID); err != nil {
			return err
		}
	}
	q := `SELECT calendar_ids,resolved_at FROM projection_mutation_operations WHERE operation_id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR UPDATE`
	}
	var stored string
	var resolved sql.NullTime
	err = tx.QueryRowContext(ctx, s.query(q), operationID).Scan(&stored, &resolved)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (stored != encoded || resolved.Valid) {
		return ErrProjectionMutationManyConflict
	}
	if err != nil {
		return err
	}
	if err = s.lockMutationConnectionsAndCalendars(ctx, tx, ids); err != nil {
		return err
	}
	for _, id := range ids {
		var eligible bool
		err = tx.QueryRowContext(ctx, s.query(`SELECT c.enabled AND c.can_read AND a.status='connected'
		FROM calendars c JOIN connections a ON a.id=c.connection_id WHERE c.id=?`), id).Scan(&eligible)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !eligible {
			return ErrCalendarSyncIneligible
		}
		if err != nil {
			return err
		}
	}
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET cursor='',generation=generation+1,status='pending',next_sync_at=?,last_error_code=NULL,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE calendar_id=?`), now.UTC(), now.UTC(), id)
		if err != nil {
			return fmt.Errorf("schedule fenced replacement: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrCalendarSyncIneligible
		}
	}
	var existing int64
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_mutation_intents WHERE mutation_id=?`), ":"+operationID).Scan(&existing); err != nil {
		return err
	}
	if existing != int64(len(ids)) {
		return ErrProjectionMutationManyConflict
	}
	for _, id := range ids {
		if err = advanceProjectionMutationGuard(ctx, s, tx, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operation_members WHERE operation_id=?`), operationID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != int64(len(ids)) {
		return ErrProjectionMutationManyConflict
	}
	for _, id := range ids {
		res, err = tx.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_intents WHERE calendar_id=? AND mutation_id=?`), id, ":"+operationID)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrProjectionMutationManyConflict
		}
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_operations SET resolved_at=? WHERE operation_id=?`), now.UTC(), operationID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit fenced replacement scheduling: %w", err)
	}
	return nil
}
