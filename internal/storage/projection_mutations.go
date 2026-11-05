package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const maxPendingProjectionMutations = 1024

var ErrProjectionMutationLimit = errors.New("too many unresolved projection mutations")

// ProjectionMutationState is diagnostic fencing information, not proof that
// the event cache is complete, fresh, or safe to consume.
type ProjectionMutationState struct {
	Epoch   int64
	Pending int64
}

// beginProjectionMutation is a storage test primitive; it must commit before calling a provider. mutationID
// must be a unique, random opaque identifier, never a payload or credential.
// IDs must not be reused after resolution. An error, including an ambiguous
// commit error, does not authorize a provider call. Intents do not expire and
// require explicit resolution.
func (s *Store) beginProjectionMutation(ctx context.Context, calendarID, mutationID string, now time.Time) error {
	if calendarID == "" || !validProjectionMutationID(mutationID) || now.IsZero() {
		return errors.New("invalid projection mutation identity or time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin projection mutation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id, epoch) VALUES (?, 0) ON CONFLICT(calendar_id) DO NOTHING`), calendarID); err != nil {
		return fmt.Errorf("initialize projection mutation guard: %w", err)
	}
	if err = advanceProjectionMutationGuard(ctx, s, tx, calendarID); err != nil {
		return err
	}
	var count int64
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_mutation_intents WHERE calendar_id=?`), calendarID).Scan(&count); err != nil {
		return fmt.Errorf("count projection mutation intents: %w", err)
	}
	if count >= maxPendingProjectionMutations {
		return ErrProjectionMutationLimit
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_intents(calendar_id, mutation_id, started_at) VALUES (?, ?, ?)`), calendarID, mutationID, now.UTC()); err != nil {
		return fmt.Errorf("insert projection mutation intent: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit projection mutation intent: %w", err)
	}
	return nil
}

// resolveProjectionMutation is a storage test primitive that removes one intent only after the caller has
// established the provider outcome or reconciled it. It does not establish a
// complete or fresh cache. Unknown outcomes must remain unresolved.
func (s *Store) resolveProjectionMutation(ctx context.Context, calendarID, mutationID string) error {
	if calendarID == "" || !validProjectionMutationID(mutationID) {
		return errors.New("invalid projection mutation identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin projection mutation resolution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = advanceProjectionMutationGuard(ctx, s, tx, calendarID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_intents WHERE calendar_id=? AND mutation_id=?`), calendarID, mutationID)
	if err != nil {
		return fmt.Errorf("resolve projection mutation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit projection mutation resolution: %w", err)
	}
	return nil
}

// InspectProjectionMutations reports a point-in-time diagnostic. A missing
// guard returns ErrNotFound. It is not a publication or an atomic authorization.
func (s *Store) InspectProjectionMutations(ctx context.Context, calendarID string) (ProjectionMutationState, error) {
	var state ProjectionMutationState
	err := s.db.QueryRowContext(ctx, s.query(`SELECT g.epoch, (SELECT COUNT(*) FROM projection_mutation_intents i WHERE i.calendar_id=g.calendar_id) FROM projection_mutation_guards g WHERE g.calendar_id=?`), calendarID).Scan(&state.Epoch, &state.Pending)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrNotFound
	}
	if err != nil {
		return state, fmt.Errorf("inspect projection mutations: %w", err)
	}
	return state, nil
}

// HasProjectionMutation supports reconciliation of a known intent ID after an
// ambiguous storage result. Presence alone never authorizes a provider call.
func (s *Store) HasProjectionMutation(ctx context.Context, calendarID, mutationID string) (bool, error) {
	if calendarID == "" || !validProjectionMutationID(mutationID) {
		return false, errors.New("invalid projection mutation identity")
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_mutation_intents WHERE calendar_id=? AND mutation_id=?)`), calendarID, mutationID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect projection mutation intent: %w", err)
	}
	return exists, nil
}

// The write locks the guard row until transaction completion on both dialects.
// A future publisher must lock this same row in its final transaction before
// checking epoch and pending intents; a diagnostic read alone is insufficient.
func advanceProjectionMutationGuard(ctx context.Context, s *Store, tx *sql.Tx, calendarID string) error {
	res, err := tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_guards SET epoch=epoch+1 WHERE calendar_id=?`), calendarID)
	if err != nil {
		return fmt.Errorf("advance projection mutation guard: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

func validProjectionMutationID(id string) bool {
	if len(id) < 16 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
