package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const maxProjectionMutationCalendars = 32

var ErrProjectionMutationManyConflict = errors.New("projection mutation operation is unavailable or differs from its original calendars")

func normalizeMutationCalendars(ids []string) ([]string, string, error) {
	if len(ids) == 0 || len(ids) > 128 {
		return nil, "", ErrProjectionMutationManyConflict
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || len(id) > 1024 || strings.ContainsRune(id, '\x00') {
			return nil, "", ErrProjectionMutationManyConflict
		}
		seen[id] = struct{}{}
	}
	if len(seen) > maxProjectionMutationCalendars {
		return nil, "", ErrProjectionMutationManyConflict
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, "", ErrProjectionMutationManyConflict
	}
	return result, string(encoded), nil
}

// BeginProjectionMutationMany persists one shared opaque operation over all
// canonical calendars in one transaction before any provider write. A failed
// or ambiguous result never authorizes the write; operation IDs are retained
// after resolution to prevent accidental reuse. The caller must independently
// prove every affected calendar was included and install this guard on all
// provider dispatch paths before relying on it for snapshot correctness.
func (s *Store) BeginProjectionMutationMany(ctx context.Context, calendarIDs []string, operationID string, now time.Time) error {
	ids, encoded, err := normalizeMutationCalendars(calendarIDs)
	if err != nil || !validProjectionMutationID(operationID) || now.IsZero() {
		return ErrProjectionMutationManyConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin multi-calendar mutation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operations(operation_id,calendar_ids,started_at) VALUES(?,?,?)`), operationID, encoded, now.UTC()); err != nil {
		return fmt.Errorf("create multi-calendar operation: %w", err)
	}
	if err = lockMutationCalendars(ctx, s, tx, ids); err != nil {
		return err
	}
	var existing int64
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_mutation_intents WHERE mutation_id=?`), ":"+operationID).Scan(&existing); err != nil {
		return fmt.Errorf("check multi-calendar operation ID: %w", err)
	}
	if existing != 0 {
		return ErrProjectionMutationManyConflict
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0) ON CONFLICT(calendar_id) DO NOTHING`), id); err != nil {
			return fmt.Errorf("initialize multi-calendar guard: %w", err)
		}
		if err = advanceProjectionMutationGuard(ctx, s, tx, id); err != nil {
			return err
		}
		var count int64
		if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_mutation_intents WHERE calendar_id=?`), id).Scan(&count); err != nil {
			return fmt.Errorf("count multi-calendar intents: %w", err)
		}
		if count >= maxPendingProjectionMutations {
			return ErrProjectionMutationLimit
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_intents(calendar_id,mutation_id,started_at) VALUES(?,?,?)`), id, ":"+operationID, now.UTC()); err != nil {
			return fmt.Errorf("create multi-calendar intent: %w", err)
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operation_members(operation_id,calendar_id,intent_id) VALUES(?,?,?)`), operationID, id, ":"+operationID); err != nil {
			return fmt.Errorf("protect multi-calendar intent: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit multi-calendar mutation: %w", err)
	}
	return nil
}

func lockMutationCalendars(ctx context.Context, s *Store, tx *sql.Tx, ids []string) error {
	for _, id := range ids {
		if s.dialect == DialectSQLite {
			result, err := tx.ExecContext(ctx, s.query(`UPDATE calendars SET enabled=enabled WHERE id=?`), id)
			if err != nil {
				return fmt.Errorf("lock mutation calendar: %w", err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrProjectionMutationManyConflict
			}
		} else {
			var found string
			err := tx.QueryRowContext(ctx, s.query(`SELECT id FROM calendars WHERE id=? FOR NO KEY UPDATE`), id).Scan(&found)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrProjectionMutationManyConflict
			}
			if err != nil {
				return fmt.Errorf("lock mutation calendar: %w", err)
			}
		}
	}
	return nil
}

// Lock the entire connection set before the calendar rows, in stable order,
// and verify every input binding after locking. Authority changes already
// take connection locks before they modify calendars and sync state.
func (s *Store) lockMutationConnectionsAndCalendars(ctx context.Context, tx *sql.Tx, ids []string) error {
	bindings := make(map[string]string, len(ids))
	connectionSet := map[string]struct{}{}
	for _, id := range ids {
		var connectionID string
		if err := tx.QueryRowContext(ctx, s.query(`SELECT connection_id FROM calendars WHERE id=?`), id).Scan(&connectionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrProjectionMutationManyConflict
			}
			return err
		}
		bindings[id] = connectionID
		connectionSet[connectionID] = struct{}{}
	}
	connections := make([]string, 0, len(connectionSet))
	for id := range connectionSet {
		connections = append(connections, id)
	}
	sort.Strings(connections)
	for _, id := range connections {
		query := `SELECT id FROM connections WHERE id=?`
		if s.dialect == DialectPostgres {
			query += ` FOR NO KEY UPDATE`
		}
		var found string
		if err := tx.QueryRowContext(ctx, s.query(query), id).Scan(&found); err != nil {
			return err
		}
	}
	if err := lockMutationCalendars(ctx, s, tx, ids); err != nil {
		return err
	}
	for _, id := range ids {
		var connectionID string
		if err := tx.QueryRowContext(ctx, s.query(`SELECT connection_id FROM calendars WHERE id=?`), id).Scan(&connectionID); err != nil {
			return err
		}
		if connectionID != bindings[id] {
			return ErrAuthorityChanged
		}
	}
	return nil
}

// ProjectionMutationManyState is a diagnostic point-in-time view. An absent
// operation does not rule out a concurrent in-flight commit, and Pending is
// never evidence that a provider call was or was not sent.
type ProjectionMutationManyState struct {
	CalendarIDs []string
	StartedAt   time.Time
	ResolvedAt  *time.Time
}

func (s *Store) GetProjectionMutationMany(ctx context.Context, operationID string) (ProjectionMutationManyState, error) {
	var out ProjectionMutationManyState
	if !validProjectionMutationID(operationID) {
		return out, ErrProjectionMutationManyConflict
	}
	var raw string
	var resolved sql.NullTime
	err := s.db.QueryRowContext(ctx, s.query(`SELECT calendar_ids,started_at,resolved_at FROM projection_mutation_operations WHERE operation_id=?`), operationID).Scan(&raw, &out.StartedAt, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, fmt.Errorf("read multi-calendar operation: %w", err)
	}
	if err = json.Unmarshal([]byte(raw), &out.CalendarIDs); err != nil {
		return ProjectionMutationManyState{}, fmt.Errorf("decode multi-calendar operation: %w", err)
	}
	_, canonical, err := normalizeMutationCalendars(out.CalendarIDs)
	if err != nil || canonical != raw {
		return ProjectionMutationManyState{}, ErrProjectionMutationManyConflict
	}
	if resolved.Valid {
		value := resolved.Time
		out.ResolvedAt = &value
	}
	return out, nil
}
