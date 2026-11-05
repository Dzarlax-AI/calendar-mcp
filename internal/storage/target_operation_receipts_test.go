package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func ensureTargetReceiptTestSchema(t *testing.T, s *Store) {
	t.Helper()
	q := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='target_operation_receipts')`
	if s.dialect == DialectPostgres {
		q = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='target_operation_receipts')`
	}
	var exists bool
	if err := s.db.QueryRowContext(t.Context(), q).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		return
	}
	script, err := migrations.ReadFile(fmt.Sprintf("migrations/%s/022_target_operation_receipts.sql", s.dialect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteTargetReceiptContract(t *testing.T) {
	s := newSQLiteStore(t)
	ensureTargetReceiptTestSchema(t, s)
	runTargetReceiptContract(t, s)
}
func runTargetReceiptContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	id := fmt.Sprintf("receipt-test-%d", now.UnixNano())
	source, target := id+"-source", id+"-target"
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM target_operation_receipts WHERE operation_id LIKE ?`), id+"%")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM target_operation_intents WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, cal := range []string{source, target} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: cal, ConnectionID: id, ProviderCalendarID: cal, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateRule(ctx, Rule{ID: id, SourceCalendarID: source, TargetCalendarID: target, State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	makeIntent := func(op, kind, event, identity string) {
		t.Helper()
		expected := ""
		if kind == "update" {
			expected = "expected"
		}
		_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO target_operation_intents(operation_id,rule_id,source_calendar_id,target_calendar_id,source_revision,window_start,window_end,identity_hash,target_event_id,expected_etag,ownership_marker,payload_hash,operation_kind,rule_version,job_id,job_attempt,worker_id,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), op, id, source, target, 1, now.Add(-time.Hour), now.Add(time.Hour), stagingDigest(identity), event, expected, "marker", stagingDigest("request"), kind, 1, "job", 1, "worker", "pending", now)
		if err != nil {
			t.Fatal(err)
		}
	}
	op := id + "-create-operation"
	makeIntent(op, "create", "", "identity-1")
	receipt := TargetOperationReceipt{OperationID: op, RequestHash: stagingDigest("request"), Kind: "create", TargetCalendarID: target, TargetEventID: "event", OwnershipMarker: "marker", ResponseETag: "etag", RecordedAt: now}
	for _, mutate := range []func(*TargetOperationReceipt){func(r *TargetOperationReceipt) { r.RequestHash = stagingDigest("wrong") }, func(r *TargetOperationReceipt) { r.TargetCalendarID = "wrong" }, func(r *TargetOperationReceipt) { r.OwnershipMarker = "wrong" }, func(r *TargetOperationReceipt) { r.ResponseETag = "" }, func(r *TargetOperationReceipt) { r.Kind = "delete" }} {
		bad := receipt
		mutate(&bad)
		if err := s.RecordTargetOperationReceipt(ctx, bad); !errors.Is(err, ErrTargetReceiptConflict) {
			t.Fatalf("invalid receipt: %v", err)
		}
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- s.RecordTargetOperationReceipt(ctx, receipt) }()
	}
	success, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			success++
		} else if errors.Is(err, ErrTargetReceiptConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("receipt race=%d/%d", success, conflicts)
	}
	got, err := s.GetTargetOperationReceipt(ctx, op)
	if err != nil || got.TargetEventID != "event" || got.RequestHash != receipt.RequestHash {
		t.Fatalf("receipt lookup: %v", err)
	}
	state, err := s.GetTargetIntentStatus(ctx, op)
	if err != nil || state.Status != "pending" {
		t.Fatalf("intent changed: %v", err)
	}
	op2 := id + "-update-operation"
	makeIntent(op2, "update", "expected-event", "identity-2")
	update := receipt
	update.OperationID = op2
	update.Kind = "update"
	update.ExpectedETag = "expected"
	update.TargetEventID = "wrong-event"
	if err := s.RecordTargetOperationReceipt(ctx, update); !errors.Is(err, ErrTargetReceiptConflict) {
		t.Fatalf("wrong event accepted: %v", err)
	}
	update.TargetEventID = "expected-event"
	if err := s.RecordTargetOperationReceipt(ctx, update); err != nil {
		t.Fatal(err)
	}
	op3 := id + "-ambiguous-operation"
	makeIntent(op3, "create", "", "identity-3")
	if err := s.MarkTargetIntentAmbiguous(ctx, op3, now); err != nil {
		t.Fatal(err)
	}
	unknown := receipt
	unknown.OperationID = op3
	if err := s.RecordTargetOperationReceipt(ctx, unknown); !errors.Is(err, ErrTargetReceiptConflict) {
		t.Fatalf("ambiguous intent accepted: %v", err)
	}
}
