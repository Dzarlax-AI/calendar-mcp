package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrOperatorRepairAuthorization = errors.New("operator repair authorization is unavailable or changed")

// BeginOperatorRepairMutation atomically consumes the exact ETag-bound repair
// authorization and creates a durable one-calendar Many-intent plus private
// exact object/ETag identity under the current calendar lease. It requires a
// connected Google source that remains enabled and readable under row locks. The returned success is not permission to call a
// provider: safe integration still needs a durable dispatch/outcome protocol,
// including confirmed no-op, unknown outcome, and fencing after lease changes.
// Lease validity is checked against the server clock after lock waits; the
// now argument is retained for compatibility and is never used to extend it.
// On ambiguous commit do not call the provider or retry blindly; inspect the
// operation ledger by the supplied opaque operationID. No runtime uses this.
func (s *Store) BeginOperatorRepairMutation(ctx context.Context, state CalendarSyncState, objectID, etag, operationID string, now time.Time) error {
	started := time.Now().UTC()
	if state.CalendarID == "" || state.LeaseOwner == "" || objectID == "" || len(objectID) > 2048 || etag == "" || len(etag) > 1024 || !validProjectionMutationID(operationID) || now.IsZero() {
		return ErrOperatorRepairAuthorization
	}
	_, encoded, err := normalizeMutationCalendars([]string{state.CalendarID})
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Ledger identity first, then shared calendar -> connection -> lease ->
	// guard -> quarantine order. Ledger insertion rolls back with every failure.
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operations(operation_id,calendar_ids,started_at) VALUES(?,?,?)`), operationID, encoded, started); err != nil {
		return fmt.Errorf("create operator repair mutation: %w", err)
	}
	if err = lockProjectionPageSource(ctx, s, tx, state.CalendarID); err != nil {
		return err
	}
	var enabled, readable bool
	var provider, connectionStatus string
	err = tx.QueryRowContext(ctx, s.query(`SELECT c.enabled,c.can_read,conn.provider,conn.status FROM calendars c JOIN connections conn ON conn.id=c.connection_id WHERE c.id=?`), state.CalendarID).Scan(&enabled, &readable, &provider, &connectionStatus)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (!enabled || !readable || provider != "google" || connectionStatus != "connected") {
		return ErrCalendarSyncIneligible
	}
	if err != nil {
		return err
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, time.Now().UTC()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0) ON CONFLICT(calendar_id) DO NOTHING`), state.CalendarID); err != nil {
		return err
	}
	if err = advanceProjectionMutationGuard(ctx, s, tx, state.CalendarID); err != nil {
		return err
	}
	var count int64
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_mutation_intents WHERE calendar_id=?`), state.CalendarID).Scan(&count); err != nil {
		return err
	}
	if count >= maxPendingProjectionMutations {
		return ErrProjectionMutationLimit
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_intents(calendar_id,mutation_id,started_at) VALUES(?,?,?)`), state.CalendarID, ":"+operationID, started); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operation_members(operation_id,calendar_id,intent_id) VALUES(?,?,?)`), operationID, state.CalendarID, ":"+operationID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_quarantine SET provider_mutation_authorized_etag='' WHERE calendar_id=? AND object_id=? AND active=? AND etag=? AND provider_mutation_authorized_etag=?`), state.CalendarID, objectID, true, etag, etag)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrOperatorRepairAuthorization
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO operator_repair_mutations(operation_id,calendar_id,object_id,etag) VALUES(?,?,?,?)`), operationID, state.CalendarID, objectID, etag); err != nil {
		return fmt.Errorf("store operator repair mutation identity: %w", err)
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, time.Now().UTC()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit operator repair mutation: %w", err)
	}
	return nil
}

// OperatorRepairMutationIdentity is private provider metadata. Never log or
// expose it outside an authenticated reconciliation workflow.
type OperatorRepairMutationIdentity struct {
	OperationID, CalendarID, ObjectID, ETag string
	ResolvedAt                              *time.Time
}

// GetOperatorRepairMutation gives exact diagnostic identity after ambiguous
// commit. A pending record alone never proves whether a provider call was sent.
func (s *Store) GetOperatorRepairMutation(ctx context.Context, operationID string) (OperatorRepairMutationIdentity, error) {
	var out OperatorRepairMutationIdentity
	if !validProjectionMutationID(operationID) {
		return out, ErrOperatorRepairAuthorization
	}
	out.OperationID = operationID
	var resolved sql.NullTime
	err := s.db.QueryRowContext(ctx, s.query(`SELECT m.calendar_id,m.object_id,m.etag,o.resolved_at FROM operator_repair_mutations m JOIN projection_mutation_operations o ON o.operation_id=m.operation_id WHERE m.operation_id=?`), operationID).Scan(&out.CalendarID, &out.ObjectID, &out.ETag, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return OperatorRepairMutationIdentity{}, ErrNotFound
	}
	if err != nil {
		return OperatorRepairMutationIdentity{}, err
	}
	if resolved.Valid {
		value := resolved.Time
		out.ResolvedAt = &value
	}
	return out, nil
}
