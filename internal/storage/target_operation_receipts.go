package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrTargetReceiptConflict = errors.New("target receipt unavailable or differs from pending intent")

// TargetOperationReceipt is an unverified, private caller assertion about a
// provider response. Even a stored receipt is NOT provider evidence, does not
// establish target ownership, and cannot authorize mapping changes or retries.
// RequestHash is the immutable payload_hash of the exact pending intent;
// operation kind, target calendar, event identity and marker are bound separately.
// Delete results are deliberately unsupported until their evidence is designed.
type TargetOperationReceipt struct {
	OperationID, RequestHash, Kind                                               string
	TargetCalendarID, TargetEventID, ExpectedETag, OwnershipMarker, ResponseETag string
	RecordedAt                                                                   time.Time
}

func validReceiptText(s string, max int) bool {
	return s != "" && len(s) <= max && !strings.ContainsRune(s, '\x00')
}

// RecordTargetOperationReceipt accepts exactly one assertion for an existing
// pending intent. Every replay is rejected, even an identical one; ambiguous
// storage results must be inspected by operation ID. No provider I/O occurs.
func (s *Store) RecordTargetOperationReceipt(ctx context.Context, r TargetOperationReceipt) error {
	if !validProjectionMutationID(r.OperationID) || !targetDigest(r.RequestHash) || (r.Kind != "create" && r.Kind != "update") || !validReceiptText(r.TargetCalendarID, 1024) || !validReceiptText(r.TargetEventID, 1024) || !validReceiptText(r.OwnershipMarker, 1024) || !validReceiptText(r.ResponseETag, 1024) || r.RecordedAt.IsZero() {
		return ErrTargetReceiptConflict
	}
	if r.Kind == "create" && r.ExpectedETag != "" || r.Kind == "update" && !validReceiptText(r.ExpectedETag, 1024) {
		return ErrTargetReceiptConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// This write serializes with a concurrent mark-ambiguous and other receipts
	// on SQLite and PostgreSQL. It does not change intent state.
	res, err := tx.ExecContext(ctx, s.query(`UPDATE target_operation_intents SET status=status WHERE operation_id=? AND status='pending'`), r.OperationID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTargetReceiptConflict
	}
	var hash, kind, target, eventID, expected, marker string
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload_hash,operation_kind,target_calendar_id,target_event_id,expected_etag,ownership_marker FROM target_operation_intents WHERE operation_id=? AND status='pending'`), r.OperationID).Scan(&hash, &kind, &target, &eventID, &expected, &marker)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (hash != r.RequestHash || kind != r.Kind || target != r.TargetCalendarID || marker != r.OwnershipMarker || expected != r.ExpectedETag || (kind == "create" && eventID != "") || (kind == "update" && eventID != r.TargetEventID)) {
		return ErrTargetReceiptConflict
	}
	if err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM target_operation_receipts WHERE operation_id=?)`), r.OperationID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrTargetReceiptConflict
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO target_operation_receipts(operation_id,request_hash,operation_kind,target_calendar_id,target_event_id,expected_etag,ownership_marker,response_etag,recorded_at) VALUES(?,?,?,?,?,?,?,?,?)`), r.OperationID, r.RequestHash, r.Kind, r.TargetCalendarID, r.TargetEventID, r.ExpectedETag, r.OwnershipMarker, r.ResponseETag, r.RecordedAt.UTC())
	if err != nil {
		return fmt.Errorf("record target receipt: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit target receipt: %w", err)
	}
	return nil
}

// GetTargetOperationReceipt is a private point-in-time diagnostic, not an
// authority to repeat operations or to transition the target intent.
func (s *Store) GetTargetOperationReceipt(ctx context.Context, operationID string) (TargetOperationReceipt, error) {
	var r TargetOperationReceipt
	if !validProjectionMutationID(operationID) {
		return r, ErrTargetReceiptConflict
	}
	r.OperationID = operationID
	err := s.db.QueryRowContext(ctx, s.query(`SELECT request_hash,operation_kind,target_calendar_id,target_event_id,expected_etag,ownership_marker,response_etag,recorded_at FROM target_operation_receipts WHERE operation_id=?`), operationID).Scan(&r.RequestHash, &r.Kind, &r.TargetCalendarID, &r.TargetEventID, &r.ExpectedETag, &r.OwnershipMarker, &r.ResponseETag, &r.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TargetOperationReceipt{}, ErrNotFound
	}
	if err != nil {
		return TargetOperationReceipt{}, err
	}
	return r, nil
}
