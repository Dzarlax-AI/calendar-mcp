package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestSQLiteOperatorRepairMutation(t *testing.T) {
	s := newSQLiteStore(t)
	ensureMutationManySchema(t, s)
	ensureOperatorRepairMetadata(t, s)
	runOperatorRepairMutationContract(t, s)
}
func ensureOperatorRepairMetadata(t *testing.T, s *Store) {
	t.Helper()
	q := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='operator_repair_mutations')`
	if s.dialect == DialectPostgres {
		q = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='operator_repair_mutations')`
	}
	var found bool
	if err := s.db.QueryRowContext(t.Context(), q).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found {
		return
	}
	script, err := migrations.ReadFile(fmt.Sprintf("migrations/%s/020_operator_repair_metadata.sql", s.dialect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
}

func runOperatorRepairMutationContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	id := fmt.Sprintf("repair-op-%d", now.UnixNano())
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM operator_repair_mutations WHERE calendar_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_operation_members WHERE calendar_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_intents WHERE calendar_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_operations WHERE operation_id LIKE ?`), id+"%")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: id, ProviderCalendarID: id, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.ClaimDueCalendarSync(ctx, "repair-worker", now, now.Add(time.Hour))
	if err != nil || lease == nil || lease.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	if err := s.ApplyEventSyncPage(ctx, *lease, EventSyncBatch{Warnings: []EventSyncWarning{{ObjectID: "synthetic-object", ETag: "etag-1", ErrorCode: string(calendar.EventSyncProtocol)}}}, false, now); err != nil {
		t.Fatal(err)
	}
	if s.dialect == DialectPostgres {
		verifyRepairScheduleLockOrder(t, s, id, now)
		if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET next_sync_at=? WHERE calendar_id=?`), now.Add(3*time.Hour), id); err != nil {
			t.Fatal(err)
		}
		verifyRepairScheduleLockOrder(t, s, id, now.Add(2*time.Hour))
	}
	if _, err := s.ScheduleEventSyncObjectRepair(ctx, id, "synthetic-object", "etag-1", now); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		query string
		value any
	}{
		{`UPDATE calendars SET enabled=? WHERE id=?`, false},
		{`UPDATE calendars SET can_read=? WHERE id=?`, false},
		{`UPDATE connections SET status=? WHERE id=?`, "pending"},
		{`UPDATE connections SET provider=? WHERE id=?`, "apple"},
	} {
		if _, err := s.db.ExecContext(ctx, s.query(change.query), change.value, id); err != nil {
			t.Fatal(err)
		}
		err := s.BeginOperatorRepairMutation(ctx, *lease, "synthetic-object", "etag-1", id+"-ineligible-operation", now)
		if !errors.Is(err, ErrCalendarSyncIneligible) {
			t.Fatalf("ineligible accepted: %v", err)
		}
		var auth string
		if err := s.db.QueryRowContext(ctx, s.query(`SELECT provider_mutation_authorized_etag FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?`), id, "synthetic-object").Scan(&auth); err != nil || auth != "etag-1" {
			t.Fatalf("ineligible consumed authorization: %v", err)
		}
		var restored any = true
		if change.query == `UPDATE connections SET status=? WHERE id=?` {
			restored = "connected"
		}
		if change.query == `UPDATE connections SET provider=? WHERE id=?` {
			restored = "google"
		}
		if _, err := s.db.ExecContext(ctx, s.query(change.query), restored, id); err != nil {
			t.Fatal(err)
		}
	}
	// A stale caller clock must not make an already-expired lease appear live.
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET lease_until=? WHERE calendar_id=?`), now.Add(-time.Minute), id); err != nil {
		t.Fatal(err)
	}
	expiredOp := id + "-expired-operation"
	if err := s.BeginOperatorRepairMutation(ctx, *lease, "synthetic-object", "etag-1", expiredOp, now.Add(-2*time.Hour)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale caller clock accepted expired lease: %v", err)
	}
	if _, err := s.GetProjectionMutationMany(ctx, expiredOp); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired attempt left partial operation: %v", err)
	}
	var stillAuthorized string
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT provider_mutation_authorized_etag FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?`), id, "synthetic-object").Scan(&stillAuthorized); err != nil || stillAuthorized != "etag-1" {
		t.Fatalf("expired attempt consumed authorization: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET lease_until=? WHERE calendar_id=?`), now.Add(time.Hour), id); err != nil {
		t.Fatal(err)
	}
	badOp := id + "-bad-operation"
	if err := s.BeginOperatorRepairMutation(ctx, *lease, "synthetic-object", "stale", badOp, now); !errors.Is(err, ErrOperatorRepairAuthorization) {
		t.Fatalf("stale ETag: %v", err)
	}
	var auth string
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT provider_mutation_authorized_etag FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?`), id, "synthetic-object").Scan(&auth); err != nil || auth != "etag-1" {
		t.Fatalf("authorization lost: %v", err)
	}
	if _, err := s.GetProjectionMutationMany(ctx, badOp); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial operation survived: %v", err)
	}
	stale := *lease
	stale.Generation++
	if err := s.BeginOperatorRepairMutation(ctx, stale, "synthetic-object", "etag-1", id+"-stale-operation", now); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale lease: %v", err)
	}
	// Two workers observing the same authorization cannot both consume it.
	type result struct {
		id  string
		err error
	}
	ch := make(chan result, 2)
	for i := 0; i < 2; i++ {
		op := fmt.Sprintf("%s-operation-%d", id, i)
		go func() {
			ch <- result{op, s.BeginOperatorRepairMutation(ctx, *lease, "synthetic-object", "etag-1", op, now)}
		}()
	}
	var winner string
	success, blocked := 0, 0
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err == nil {
			success++
			winner = r.id
		} else if errors.Is(r.err, ErrOperatorRepairAuthorization) {
			blocked++
		} else {
			t.Fatalf("concurrent attempt: %v", r.err)
		}
	}
	if success != 1 || blocked != 1 {
		t.Fatalf("success=%d blocked=%d", success, blocked)
	}
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT provider_mutation_authorized_etag FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?`), id, "synthetic-object").Scan(&auth); err != nil || auth != "" {
		t.Fatalf("authorization not consumed: %v", err)
	}
	op, err := s.GetProjectionMutationMany(ctx, winner)
	if err != nil || op.ResolvedAt != nil || len(op.CalendarIDs) != 1 || op.CalendarIDs[0] != id {
		t.Fatalf("durable operation missing: %+v %v", op, err)
	}
	identity, err := s.GetOperatorRepairMutation(ctx, winner)
	if err != nil || identity.CalendarID != id || identity.ObjectID != "synthetic-object" || identity.ETag != "etag-1" || identity.ResolvedAt != nil {
		t.Fatalf("exact private identity not retained: %v", err)
	}
	guard, err := s.InspectProjectionMutations(ctx, id)
	if err != nil || guard.Pending != 1 {
		t.Fatalf("pending guard: %+v %v", guard, err)
	}
	if err := s.BeginOperatorRepairMutation(ctx, *lease, "synthetic-object", "etag-1", winner, now); err == nil {
		t.Fatal("duplicate operation accepted")
	}
}

// A concurrent operator scheduler must wait for state before it can lock
// quarantine, even when the calendar lease is considered expired.
func verifyRepairScheduleLockOrder(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	holder, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	var locked string
	if err = holder.QueryRowContext(ctx, s.query(`SELECT calendar_id FROM calendar_sync_state WHERE calendar_id=? FOR UPDATE`), id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.ScheduleEventSyncObjectRepair(ctx, id, "synthetic-object", "etag-1", now)
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT calendar_id FROM calendar_sync_state%FOR NO KEY UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("operator schedule did not wait on state first")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err = holder.ExecContext(checkCtx, s.query(`UPDATE calendar_sync_quarantine SET etag=etag WHERE calendar_id=? AND object_id=?`), id, "synthetic-object"); err != nil {
		t.Fatalf("scheduler locked quarantine before state: %v", err)
	}
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operator scheduling did not complete")
	}
}

func TestOperatorRepairMutationPersistsAcrossReopen(t *testing.T) {
	ctx := t.Context()
	url := "sqlite://" + filepath.Join(t.TempDir(), "repair.db")
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureMutationManySchema(t, s)
	ensureOperatorRepairMetadata(t, s)
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, Connection{ID: "reopen-repair", Provider: "google", AccountFingerprint: "reopen-repair", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertCalendar(ctx, Calendar{ID: "reopen-repair", ConnectionID: "reopen-repair", ProviderCalendarID: "primary", Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	if err = s.ApplyEventSyncPage(ctx, *lease, EventSyncBatch{Warnings: []EventSyncWarning{{ObjectID: "object", ETag: "etag", ErrorCode: string(calendar.EventSyncProtocol)}}}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ScheduleEventSyncObjectRepair(ctx, "reopen-repair", "object", "etag", now); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginOperatorRepairMutation(ctx, *lease, "object", "etag", "reopen-repair-operation", now); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	identity, err := s.GetOperatorRepairMutation(ctx, "reopen-repair-operation")
	if err != nil || identity.CalendarID != "reopen-repair" || identity.ObjectID != "object" || identity.ETag != "etag" || identity.ResolvedAt != nil {
		t.Fatalf("repair identity after restart: %v", err)
	}
	op, err := s.GetProjectionMutationMany(ctx, "reopen-repair-operation")
	if err != nil || op.ResolvedAt != nil {
		t.Fatalf("operation did not survive restart: %v", err)
	}
	var auth string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT provider_mutation_authorized_etag FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?`), "reopen-repair", "object").Scan(&auth); err != nil || auth != "" {
		t.Fatalf("authorization changed after reopen: %v", err)
	}
	guard, err := s.InspectProjectionMutations(ctx, "reopen-repair")
	if err != nil || guard.Pending != 1 {
		t.Fatalf("pending state: %+v %v", guard, err)
	}
}
