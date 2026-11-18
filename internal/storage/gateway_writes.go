package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"calendar-mcp/internal/calendar"
	"github.com/google/uuid"
)

const gatewayDispatchStartLimit = time.Minute
const gatewayRecheckDelay = time.Minute
const gatewayWriteOutcomeTimeout = 5 * time.Minute
const gatewayReceiptBytes = 1 << 20

type GatewayWrite struct {
	ID, Phase  string
	Scope      calendar.GatewayWriteScope
	Calendars  []string
	OutcomeDue time.Time
}

type gatewayWriteReservation struct {
	store *Store
	id    string
	ids   []string
}

func (s *Store) BeginGatewayWrite(ctx context.Context, ids []string, scope calendar.GatewayWriteScope) (calendar.GatewayWriteReservation, error) {
	if s.artifactCipher == nil {
		return nil, ErrExclusiveDispatchConflict
	}
	id := uuid.NewString()
	identity := sha256.Sum256([]byte(id))
	err := s.BeginExclusiveDispatch(ctx, ExclusiveDispatchRequest{OperationID: id, TargetKey: GlobalSyncDispatchKey,
		RequestHash: hex.EncodeToString(identity[:]), Kind: "run", CalendarIDs: ids, Now: time.Now(), gatewayWrite: &scope})
	if err != nil {
		return nil, err
	}
	return &gatewayWriteReservation{store: s, id: id, ids: slices.Clone(ids)}, nil
}

func (s *Store) insertGatewayWrite(ctx context.Context, tx *sql.Tx, id string, scope calendar.GatewayWriteScope, ids []string, now time.Time) error {
	switch scope.Kind {
	case "create", "import", "update", "delete", "move", "respond":
	default:
		return ErrExclusiveDispatchConflict
	}
	if !slices.Contains(ids, scope.CalendarID) || scope.Kind == "move" && !slices.Contains(ids, scope.DestinationCalendarID) || len(scope.EventID) > 1024 {
		return ErrExclusiveDispatchConflict
	}
	var revision string
	err := tx.QueryRowContext(ctx, s.query(`SELECT a.authorization_revision FROM calendars c JOIN connections a ON a.id=c.connection_id
	WHERE c.id=? AND c.enabled=TRUE AND a.status='connected'`), scope.CalendarID).Scan(&revision)
	if err != nil {
		return err
	}
	if revision == "" || scope.AuthorizationRevision != "" && scope.AuthorizationRevision != revision {
		return ErrAuthorityChanged
	}
	scope.AuthorizationRevision = revision
	value, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	grants := map[string]string{}
	for _, calendarID := range ids {
		current, err := s.gatewayCalendarGrants(ctx, tx, now, calendarID, false)
		if err != nil {
			return err
		}
		if len(current) == 1 && current[0].Class != GatewayMetadataOnly && now.Before(current[0].HardVerifiedUntil) {
			grants[calendarID] = current[0].Revision
		}
	}
	encodedGrants, err := json.Marshal(grants)
	if err != nil {
		return err
	}
	due := now.Add(gatewayDispatchStartLimit).UnixMilli()
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO gateway_writes(operation_id,scope_json,grants_json,phase,dispatch_by_ms,next_check_ms)
	VALUES (?,?,?,'reserved',?,?)`), id, string(value), string(encodedGrants), due, due)
	return err
}

func (r *gatewayWriteReservation) ID() string { return r.id }

func (r *gatewayWriteReservation) Authorize(ctx context.Context) error {
	now := time.Now()
	var raw string
	if err := r.store.db.QueryRowContext(ctx, r.store.query(`SELECT scope_json FROM gateway_writes WHERE operation_id=?`), r.id).Scan(&raw); err != nil {
		return err
	}
	var scope calendar.GatewayWriteScope
	if err := json.Unmarshal([]byte(raw), &scope); err != nil {
		return err
	}
	res, err := r.store.db.ExecContext(ctx, r.store.query(`UPDATE gateway_writes SET phase='dispatched',next_check_ms=?,outcome_due_ms=?
	WHERE operation_id=? AND phase='reserved' AND dispatch_by_ms>?
	AND EXISTS(SELECT 1 FROM calendars c JOIN connections a ON a.id=c.connection_id
	WHERE c.id=? AND c.enabled=TRUE AND a.status='connected' AND a.authorization_revision=?)`),
		now.Add(gatewayRecheckDelay).UnixMilli(), now.Add(gatewayWriteOutcomeTimeout).UnixMilli(), r.id, now.UnixMilli(), scope.CalendarID, scope.AuthorizationRevision)
	return oneGatewayAuthorityRow(res, err)
}

func (r *gatewayWriteReservation) Complete(ctx context.Context, result any, notSent bool) error {
	now := time.Now()
	if notSent {
		if err := r.store.recordGatewayNotSent(ctx, r.id, false, now); err != nil {
			return err
		}
	} else if err := r.store.RecordGatewayWriteConfirmation(ctx, r.id, result, now); err != nil {
		return err
	}
	return r.store.FinishGatewayWrite(ctx, r.id, r.ids, now)
}

func (s *Store) RecordGatewayWriteConfirmation(ctx context.Context, id string, result any, now time.Time) error {
	if result == nil || s.artifactCipher == nil {
		return ErrExclusiveDispatchConflict
	}
	value, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(value) > gatewayReceiptBytes || string(value) == "null" {
		return ErrGatewayQueryTooLarge
	}
	ciphertext, err := s.artifactCipher.Encrypt(value, []byte("gateway-write-receipt:"+id))
	if err != nil {
		return err
	}
	// PostgreSQL may evaluate an UPDATE predicate before it waits for the
	// row lock. Take that lock first, then compare with the database clock.
	// SQLite takes the write lock before evaluating the UPDATE.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	dbNow := "(CAST(strftime('%s','now') AS INTEGER) * 1000 + CAST(substr(strftime('%f','now'),4,3) AS INTEGER))"
	if s.dialect == DialectPostgres {
		var locked string
		if err := tx.QueryRowContext(ctx, s.query(`SELECT operation_id FROM gateway_writes WHERE operation_id=? FOR UPDATE`), id).Scan(&locked); err != nil {
			return err
		}
		dbNow = "FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)"
	}
	query := `UPDATE gateway_writes SET phase='confirmed',confirmed_ms=?,receipt_ciphertext=?,next_check_ms=?
	WHERE operation_id=? AND phase='dispatched' AND outcome_due_ms>? AND outcome_due_ms>` + dbNow
	res, err := tx.ExecContext(ctx, s.query(query), now.UnixMilli(), base64.StdEncoding.EncodeToString(ciphertext), now.UnixMilli(), id, now.UnixMilli())
	if err := oneGatewayAuthorityRow(res, err); err != nil {
		if !errors.Is(err, ErrAuthorityChanged) {
			return err
		}
		var phase string
		err = tx.QueryRowContext(ctx, s.query(`SELECT phase FROM gateway_writes WHERE operation_id=?`), id).Scan(&phase)
		if err != nil {
			return err
		}
		if phase != "confirmed" {
			return ErrAuthorityChanged
		}
	}
	return tx.Commit()
}

func (s *Store) recordGatewayNotSent(ctx context.Context, id string, expired bool, now time.Time) error {
	query := `UPDATE gateway_writes SET phase='not_dispatched',next_check_ms=? WHERE operation_id=? AND phase='dispatched'`
	args := []any{now.UnixMilli(), id}
	if expired {
		query = `UPDATE gateway_writes SET phase='not_dispatched',next_check_ms=? WHERE operation_id=? AND phase='reserved' AND dispatch_by_ms<=?`
		args = append(args, now.UnixMilli())
	}
	res, err := s.db.ExecContext(ctx, s.query(query), args...)
	return oneGatewayAuthorityRow(res, err)
}

// ClaimGatewayWriteCheck limits provider lookup attempts. Every recovery
// path starts here and invokes read methods or local SQL exclusively.
func (s *Store) ClaimGatewayWriteCheck(ctx context.Context, now time.Time) (*GatewayWrite, error) {
	var job GatewayWrite
	var raw, ids string
	var outcomeDue int64
	err := s.db.QueryRowContext(ctx, s.query(`SELECT w.operation_id,w.phase,w.scope_json,o.calendar_ids,w.outcome_due_ms
	FROM gateway_writes w JOIN projection_mutation_operations o ON o.operation_id=w.operation_id
	WHERE o.resolved_at IS NULL AND w.next_check_ms<=? ORDER BY w.next_check_ms,w.operation_id LIMIT 1`), now.UnixMilli()).
		Scan(&job.ID, &job.Phase, &raw, &ids, &outcomeDue)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	next := now.Add(gatewayRecheckDelay).UnixMilli()
	job.OutcomeDue = time.UnixMilli(outcomeDue).UTC()
	if job.Phase == "dispatched" && now.UnixMilli() < outcomeDue {
		next = min(next, outcomeDue)
	}
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE gateway_writes SET next_check_ms=? WHERE operation_id=? AND phase=? AND next_check_ms<=?`),
		next, job.ID, job.Phase, now.UnixMilli())
	if err := oneGatewayAuthorityRow(res, err); err != nil {
		if errors.Is(err, ErrAuthorityChanged) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &job.Scope); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(ids), &job.Calendars); err != nil {
		return nil, err
	}
	if job.Phase == "reserved" {
		if err := s.recordGatewayNotSent(ctx, job.ID, true, now); err != nil {
			if errors.Is(err, ErrAuthorityChanged) {
				return nil, nil
			}
			return nil, err
		}
		job.Phase = "not_dispatched"
	}
	return &job, nil
}

// ExpireOneGatewayWrite bypasses lookup leases so a slow marker read cannot
// postpone release. The phase transition and fence release remain atomic.
func (s *Store) ExpireOneGatewayWrite(ctx context.Context, now time.Time) error {
	var write GatewayWrite
	var ids string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT w.operation_id,o.calendar_ids
	FROM gateway_writes w JOIN projection_mutation_operations o ON o.operation_id=w.operation_id
	WHERE o.resolved_at IS NULL AND w.phase='dispatched' AND w.outcome_due_ms<=?
	ORDER BY w.outcome_due_ms,w.operation_id LIMIT 1`), now.UnixMilli()).Scan(&write.ID, &ids)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(ids), &write.Calendars); err != nil {
		return err
	}
	return s.ExpireGatewayWrite(ctx, write, now)
}

func (s *Store) FinishGatewayWrite(ctx context.Context, id string, ids []string, now time.Time) error {
	return s.finishGatewayWrite(ctx, id, ids, now, s.publishGatewayWriteReceipt)
}

// ExpireGatewayWrite releases the write fence and schedules fresh syncs without
// recording any provider outcome. The phase change and release commit together.
func (s *Store) ExpireGatewayWrite(ctx context.Context, write GatewayWrite, now time.Time) error {
	return s.finishGatewayWrite(ctx, write.ID, write.Calendars, now, func(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
		res, err := tx.ExecContext(ctx, s.query(`UPDATE gateway_writes SET phase='outcome_unknown'
		WHERE operation_id=? AND phase='dispatched' AND outcome_due_ms<=?`), id, now.UnixMilli())
		return oneGatewayAuthorityRow(res, err)
	})
}

func (s *Store) finishGatewayWrite(ctx context.Context, id string, ids []string, now time.Time,
	publish func(context.Context, *sql.Tx, string, time.Time) error) error {
	err := s.releaseExclusiveDispatch(ctx, id, ids, now, publish)
	if !errors.Is(err, ErrExclusiveDispatchConflict) {
		return err
	}
	var resolved bool
	if queryErr := s.db.QueryRowContext(ctx, s.query(`SELECT o.resolved_at IS NOT NULL
	FROM gateway_writes w JOIN projection_mutation_operations o ON o.operation_id=w.operation_id
	WHERE w.operation_id=?`), id).Scan(&resolved); queryErr == nil && resolved {
		return nil
	}
	return err
}
