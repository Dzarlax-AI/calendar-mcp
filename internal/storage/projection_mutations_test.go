package storage

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestProjectionMutationContractSQLite(t *testing.T) {
	store, now := newEventCacheStore(t)
	runProjectionMutationContract(t, store, "calendar", now)
}

func TestProjectionMutationSurvivesSyncClaim(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := t.Context()
	if err := store.beginProjectionMutation(ctx, "calendar", "0123456789abcdef", now); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimDueCalendarSync(ctx, "intent-test-worker", now, now.Add(time.Hour))
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	state, err := store.InspectProjectionMutations(ctx, "calendar")
	if err != nil || state != (ProjectionMutationState{Epoch: 1, Pending: 1}) {
		t.Fatalf("claim changed guard: %+v %v", state, err)
	}
}

func TestProjectionMutationSurvivesReopen(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := t.Context()
	var seq int
	var name, path string
	if err := store.db.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := store.beginProjectionMutation(ctx, "calendar", "0123456789abcdef", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	state, err := reopened.InspectProjectionMutations(ctx, "calendar")
	if err != nil || state != (ProjectionMutationState{Epoch: 1, Pending: 1}) {
		t.Fatalf("reopened intent: %+v %v", state, err)
	}
}

func runProjectionMutationContract(t *testing.T, store *Store, calendarID string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	id := "0123456789abcdef"
	if _, err := store.InspectProjectionMutations(ctx, calendarID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing guard: %v", err)
	}
	for _, bad := range []string{"", "short", "0123456789abcdef/secret", string(make([]byte, 129))} {
		if err := store.beginProjectionMutation(ctx, calendarID, bad, now); err == nil {
			t.Fatalf("accepted bad id")
		}
	}
	if err := store.beginProjectionMutation(ctx, calendarID, id, now); err != nil {
		t.Fatal(err)
	}
	if found, err := store.HasProjectionMutation(ctx, calendarID, id); err != nil || !found {
		t.Fatalf("intent lookup: %v %v", found, err)
	}
	state, err := store.InspectProjectionMutations(ctx, calendarID)
	if err != nil || state != (ProjectionMutationState{Epoch: 1, Pending: 1}) {
		t.Fatalf("begin: %+v %v", state, err)
	}
	if err := store.beginProjectionMutation(ctx, calendarID, id, now); err == nil {
		t.Fatal("duplicate begin succeeded")
	}
	state, _ = store.InspectProjectionMutations(ctx, calendarID)
	if state != (ProjectionMutationState{Epoch: 1, Pending: 1}) {
		t.Fatalf("duplicate changed guard: %+v", state)
	}
	if err := store.resolveProjectionMutation(ctx, calendarID, "other0123456789abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown resolve: %v", err)
	}
	state, _ = store.InspectProjectionMutations(ctx, calendarID)
	if state.Epoch != 1 || state.Pending != 1 {
		t.Fatalf("rollback changed guard: %+v", state)
	}
	if err := store.resolveProjectionMutation(ctx, calendarID, id); err != nil {
		t.Fatal(err)
	}
	if found, err := store.HasProjectionMutation(ctx, calendarID, id); err != nil || found {
		t.Fatalf("resolved intent lookup: %v %v", found, err)
	}
	state, err = store.InspectProjectionMutations(ctx, calendarID)
	if err != nil || state != (ProjectionMutationState{Epoch: 2, Pending: 0}) {
		t.Fatalf("resolved: %+v %v", state, err)
	}
	if err := store.resolveProjectionMutation(ctx, calendarID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat resolve: %v", err)
	}
	state, _ = store.InspectProjectionMutations(ctx, calendarID)
	if state.Epoch != 2 {
		t.Fatalf("repeat resolve advanced epoch: %+v", state)
	}
}

func TestProjectionMutationLimitRollsBack(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	if err := s.beginProjectionMutation(ctx, "calendar", "0123456789abcdef", now); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxPendingProjectionMutations; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projection_mutation_intents(calendar_id, mutation_id, started_at) VALUES (?, ?, ?)`, "calendar", fmt.Sprintf("%016d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.beginProjectionMutation(ctx, "calendar", "new-0123456789abcdef", now); !errors.Is(err, ErrProjectionMutationLimit) {
		t.Fatalf("limit: %v", err)
	}
	state, err := s.InspectProjectionMutations(ctx, "calendar")
	if err != nil || state.Epoch != 1 || state.Pending != maxPendingProjectionMutations {
		t.Fatalf("limit rollback: %+v %v", state, err)
	}
}

// A future publisher must hold this same row lock while it checks epoch and
// pending intents and commits its publication. This verifies the lock, not a
// publication API, which does not exist yet.
func runProjectionMutationLockRace(t *testing.T, first, second *Store, calendarID string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if err := first.beginProjectionMutation(ctx, calendarID, "initial-0123456789", now); err != nil {
		t.Fatal(err)
	}
	if err := first.resolveProjectionMutation(ctx, calendarID, "initial-0123456789"); err != nil {
		t.Fatal(err)
	}
	tx, err := first.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, first.query(`UPDATE projection_mutation_guards SET epoch=epoch WHERE calendar_id=?`), calendarID); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err := tx.QueryRowContext(ctx, first.query(`SELECT epoch FROM projection_mutation_guards WHERE calendar_id=?`), calendarID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- second.beginProjectionMutation(ctx, calendarID, "concurrent-0123456789", now) }()
	select {
	case err := <-done:
		t.Fatalf("begin completed while guard locked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("begin did not resume")
	}
	state, err := first.InspectProjectionMutations(ctx, calendarID)
	if err != nil || state.Epoch != epoch+1 || state.Pending != 1 {
		t.Fatalf("race: %+v epoch=%d error=%v", state, epoch, err)
	}
}

func TestProjectionMutationGuardSerializesSQLite(t *testing.T) {
	first, now := newEventCacheStore(t)
	var seq int
	var name, path string
	if err := first.db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runProjectionMutationLockRace(t, first, second, "calendar", now)
}
