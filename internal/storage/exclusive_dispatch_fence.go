package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrExclusiveDispatchConflict = errors.New("exclusive dispatch fence unavailable or already reserved")

// GlobalSyncDispatchKey serializes worker sync runs with verified source
// publication. It is not a lock for external calendar writers.
const GlobalSyncDispatchKey = "calendar-sync-engine-global-v1"

// ExclusiveDispatchRequest is an untrusted reservation request. TargetKey is
// an opaque caller-supplied identity; this API neither resolves nor validates
// physical calendar identity or aliases. RequestHash is lowercase SHA-256.
type ExclusiveDispatchRequest struct {
	OperationID, TargetKey, RequestHash, Kind string
	CalendarIDs                               []string
	Now                                       time.Time
}

// BeginExclusiveDispatch atomically reserves one exact target key and fences
// all supplied projections. It does not authorize a provider call: identity,
// ownership, job fencing, notification safety and installation on every writer
// remain prerequisites. An error or ambiguous commit never authorizes a call.
// Reservations have no expiry or release API; an unknown outcome stays parked.
// Calling this separately from experimentalBeginTargetGuard cannot compose
// them: both create a ledger row. The separate private combined experiment
// creates its target intent and reservation in one transaction instead. Neither
// path is wired to runtime.
func (s *Store) BeginExclusiveDispatch(ctx context.Context, in ExclusiveDispatchRequest) error {
	ids, encoded, err := normalizeMutationCalendars(in.CalendarIDs)
	if err != nil || !validProjectionMutationID(in.OperationID) || !validExclusiveTargetKey(in.TargetKey) || !targetDigest(in.RequestHash) || in.Now.IsZero() || (in.Kind != "create" && in.Kind != "update" && in.Kind != "delete" && in.Kind != "run") {
		return ErrExclusiveDispatchConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operations(operation_id,calendar_ids,started_at) VALUES(?,?,?) ON CONFLICT DO NOTHING`), in.OperationID, encoded, in.Now.UTC())
	if err != nil {
		return fmt.Errorf("create exclusive dispatch ledger: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrExclusiveDispatchConflict
	}
	if err = s.insertExclusiveReservation(ctx, tx, in); err != nil {
		return err
	}
	if err = lockMutationCalendars(ctx, s, tx, ids); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0) ON CONFLICT(calendar_id) DO NOTHING`), id); err != nil {
			return err
		}
		if err = advanceProjectionMutationGuard(ctx, s, tx, id); err != nil {
			return err
		}
		var count int64
		if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_mutation_intents WHERE calendar_id=?`), id).Scan(&count); err != nil {
			return err
		}
		if count >= maxPendingProjectionMutations {
			return ErrProjectionMutationLimit
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_intents(calendar_id,mutation_id,started_at) VALUES(?,?,?)`), id, ":"+in.OperationID, in.Now.UTC()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operation_members(operation_id,calendar_id,intent_id) VALUES(?,?,?)`), in.OperationID, id, ":"+in.OperationID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO exclusive_dispatch_guard_members(operation_id,calendar_id) VALUES(?,?)`), in.OperationID, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit exclusive dispatch fence: %w", err)
	}
	return nil
}

func validExclusiveTargetKey(key string) bool {
	return key != "" && len(key) <= 512 && !strings.ContainsRune(key, '\x00')
}

// insertExclusiveReservation is only called after the matching ledger insert.
func (s *Store) insertExclusiveReservation(ctx context.Context, tx *sql.Tx, in ExclusiveDispatchRequest) error {
	res, err := tx.ExecContext(ctx, s.query(`INSERT INTO exclusive_dispatch_reservations(operation_id,target_key,request_hash,operation_kind,created_at) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`), in.OperationID, in.TargetKey, in.RequestHash, in.Kind, in.Now.UTC())
	if err != nil {
		return fmt.Errorf("reserve exclusive dispatch target: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrExclusiveDispatchConflict
	}
	return nil
}
