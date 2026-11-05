package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteExclusiveDispatchFence(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "fence.db")
	first, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err = first.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runExclusiveDispatchFenceContract(t, first, second, fmt.Sprintf("exclusive-%d", time.Now().UnixNano()))
}

func runExclusiveDispatchFenceContract(t *testing.T, first, second *Store, prefix string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	a, b := prefix+"-a", prefix+"-b"
	if err := first.CreateConnection(ctx, Connection{ID: prefix, Provider: "google", AccountFingerprint: prefix, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM exclusive_dispatch_guard_members WHERE operation_id LIKE ?`), prefix+"%")
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM exclusive_dispatch_reservations WHERE operation_id LIKE ?`), prefix+"%")
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM projection_mutation_operation_members WHERE operation_id LIKE ?`), prefix+"%")
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM projection_mutation_intents WHERE calendar_id IN (?,?)`), a, b)
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM projection_mutation_operations WHERE operation_id LIKE ?`), prefix+"%")
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM calendars WHERE connection_id=?`), prefix)
		_, _ = first.db.ExecContext(ctx, first.query(`DELETE FROM connections WHERE id=?`), prefix)
	})
	for _, id := range []string{a, b} {
		if err := first.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: prefix, ProviderCalendarID: id, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	req := ExclusiveDispatchRequest{OperationID: prefix + "-first-operation", TargetKey: prefix + "-physical-target", RequestHash: projectionPageHash([]byte("request")), Kind: "create", CalendarIDs: []string{a, b}, Now: now}
	bad := req
	bad.OperationID = prefix + "-rollback-operation"
	bad.CalendarIDs = []string{a, prefix + "-missing"}
	if err := first.BeginExclusiveDispatch(ctx, bad); err == nil {
		t.Fatal("missing calendar accepted")
	}
	var count int
	if err := first.db.QueryRowContext(ctx, first.query(`SELECT COUNT(*) FROM exclusive_dispatch_reservations WHERE operation_id=?`), bad.OperationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed begin left reservation: %d %v", count, err)
	}
	if _, err := first.GetProjectionMutationMany(ctx, bad.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed begin left ledger: %v", err)
	}
	// Cause the second member insert to fail after the first member and
	// its guard have already been written inside the transaction.
	late := req
	late.OperationID = prefix + "-late-failure-operation"
	if _, err := first.db.ExecContext(ctx, first.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0)`), b); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.ExecContext(ctx, first.query(`INSERT INTO projection_mutation_intents(calendar_id,mutation_id,started_at) VALUES(?,?,?)`), b, ":"+late.OperationID, now); err != nil {
		t.Fatal(err)
	}
	if err := first.BeginExclusiveDispatch(ctx, late); err == nil {
		t.Fatal("late member conflict accepted")
	}
	if _, err := first.InspectProjectionMutations(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late failure left first guard: %v", err)
	}
	if err := first.db.QueryRowContext(ctx, first.query(`SELECT COUNT(*) FROM exclusive_dispatch_reservations WHERE operation_id=?`), late.OperationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("late failure left reservation: %d %v", count, err)
	}
	if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM projection_mutation_intents WHERE calendar_id=? AND mutation_id=?`), b, ":"+late.OperationID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		op  string
		err error
	}
	ch := make(chan result, 2)
	for i, store := range []*Store{first, second} {
		r := req
		r.OperationID = fmt.Sprintf("%s-race-operation-%d", prefix, i)
		go func() { ch <- result{r.OperationID, store.BeginExclusiveDispatch(ctx, r)} }()
	}
	var winner string
	success, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err == nil {
			success++
			winner = r.op
		} else if errors.Is(r.err, ErrExclusiveDispatchConflict) {
			conflicts++
		} else {
			t.Fatal(r.err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("race successes=%d conflicts=%d", success, conflicts)
	}
	for _, id := range []string{a, b} {
		state, err := first.InspectProjectionMutations(ctx, id)
		if err != nil || state.Pending != 1 {
			t.Fatalf("guard %s: %+v %v", id, state, err)
		}
	}
	if err := first.ResolveProjectionMutationManyAndSchedule(ctx, []string{a, b}, winner, now); err == nil {
		t.Fatal("generic resolver released exclusive fence")
	}
	for _, id := range []string{a, b} {
		state, err := first.InspectProjectionMutations(ctx, id)
		if err != nil || state.Pending != 1 {
			t.Fatalf("guard lost after resolution attempt: %+v %v", state, err)
		}
	}
	replay := req
	replay.OperationID = winner
	replay.TargetKey = prefix + "-different-target"
	if err := first.BeginExclusiveDispatch(ctx, replay); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("reused operation ID: %v", err)
	}
	other := req
	other.OperationID = prefix + "-distinct-operation"
	other.TargetKey = prefix + "-different-target"
	if err := second.BeginExclusiveDispatch(ctx, other); err != nil {
		t.Fatalf("distinct key blocked: %v", err)
	}
	// A missing/unknown provider outcome never invokes the experimental
	// release; a new operation with the same key must remain blocked.
	next := req
	next.OperationID = prefix + "-after-release"
	if err := second.BeginExclusiveDispatch(ctx, next); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("unknown outcome did not stay parked: %v", err)
	}
	if err := first.experimentalReleaseExclusiveAfterConfirmedRun(ctx, winner, []string{a}, now); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("partial release accepted: %v", err)
	}
	if _, err := first.db.ExecContext(ctx, first.query(`UPDATE calendar_sync_state SET cursor=?,status=? WHERE calendar_id IN (?,?)`), "synthetic-cursor", "ready", a, b); err != nil {
		t.Fatal(err)
	}
	if err := first.experimentalReleaseExclusiveAfterConfirmedRun(ctx, winner, []string{b, a}, now); err != nil {
		t.Fatalf("confirmed run release: %v", err)
	}
	if err := first.experimentalReleaseExclusiveAfterConfirmedRun(ctx, winner, []string{a, b}, now); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("release replay accepted: %v", err)
	}
	for _, id := range []string{a, b} {
		state, err := first.InspectProjectionMutations(ctx, id)
		if err != nil || state.Pending != 1 {
			t.Fatalf("release removed another guard: %+v %v", state, err)
		}
		var cursor, status string
		if err = first.db.QueryRowContext(ctx, first.query(`SELECT cursor,status FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&cursor, &status); err != nil || cursor != "" || status != "pending" {
			t.Fatalf("release did not schedule replacement: %q %q %v", cursor, status, err)
		}
	}
	if err := second.BeginExclusiveDispatch(ctx, next); err != nil {
		t.Fatalf("released key remains blocked: %v", err)
	}
}
