package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ReleaseExclusiveAfterConfirmedRun is only for a caller that has observed
// either no provider dispatch or a fully successful run with every mapping
// committed. This method cannot verify that assertion. Unknown outcomes must
// remain parked and must never be passed here.
func (s *Store) ReleaseExclusiveAfterConfirmedRun(ctx context.Context, operationID string, calendarIDs []string, now time.Time) error {
	return s.experimentalReleaseExclusiveAfterConfirmedRun(ctx, operationID, calendarIDs, now)
}

// experimentalReleaseExclusiveAfterConfirmedRun is an unconnected storage
// experiment. The caller must already have proved that no provider call was
// sent, or that every sent call completed and its mapping CAS committed. This
// method cannot verify either assertion; an unknown outcome must stay parked.
// It never resolves an operation protected by another guard link. It does not
// authorize runtime dispatch and is not a reconciliation protocol.
func (s *Store) experimentalReleaseExclusiveAfterConfirmedRun(ctx context.Context, operationID string, calendarIDs []string, now time.Time) error {
	ids, encoded, err := normalizeMutationCalendars(calendarIDs)
	if err != nil || !validProjectionMutationID(operationID) || now.IsZero() {
		return ErrExclusiveDispatchConflict
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
	q := `SELECT o.calendar_ids,o.resolved_at,r.released_at FROM projection_mutation_operations o JOIN exclusive_dispatch_reservations r ON r.operation_id=o.operation_id WHERE o.operation_id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR UPDATE OF o,r`
	}
	var stored string
	var resolved, released sql.NullTime
	err = tx.QueryRowContext(ctx, s.query(q), operationID).Scan(&stored, &resolved, &released)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (stored != encoded || resolved.Valid || released.Valid) {
		return ErrExclusiveDispatchConflict
	}
	if err != nil {
		return err
	}
	if err = lockMutationCalendars(ctx, s, tx, ids); err != nil {
		return err
	}
	connectionSet := map[string]struct{}{}
	for _, id := range ids {
		var connectionID string
		if err = tx.QueryRowContext(ctx, s.query(`SELECT connection_id FROM calendars WHERE id=?`), id).Scan(&connectionID); err != nil {
			return err
		}
		connectionSet[connectionID] = struct{}{}
	}
	connections := make([]string, 0, len(connectionSet))
	for id := range connectionSet {
		connections = append(connections, id)
	}
	sort.Strings(connections)
	for _, id := range connections {
		q = `SELECT id FROM connections WHERE id=?`
		if s.dialect == DialectPostgres {
			q += ` FOR NO KEY UPDATE`
		}
		var found string
		if err = tx.QueryRowContext(ctx, s.query(q), id).Scan(&found); err != nil {
			return err
		}
	}
	// Missing sync state is allowed: guard epoch still invalidates any snapshot.
	// An existing state is reset even if the calendar has since been disabled.
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET cursor='',generation=generation+1,status='pending',next_sync_at=?,last_error_code=NULL,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE calendar_id=?`), now.UTC(), now.UTC(), id); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if err = advanceProjectionMutationGuard(ctx, s, tx, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, s.query(`DELETE FROM exclusive_dispatch_guard_members WHERE operation_id=?`), operationID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != int64(len(ids)) {
		return ErrExclusiveDispatchConflict
	}
	res, err = tx.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operation_members WHERE operation_id=?`), operationID)
	if err != nil {
		return err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if n != int64(len(ids)) {
		return ErrExclusiveDispatchConflict
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
			return ErrExclusiveDispatchConflict
		}
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_operations SET resolved_at=? WHERE operation_id=?`), now.UTC(), operationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE exclusive_dispatch_reservations SET released_at=? WHERE operation_id=?`), now.UTC(), operationID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit experimental exclusive release: %w", err)
	}
	return nil
}
