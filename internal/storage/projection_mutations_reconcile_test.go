package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSQLiteResolveMutationAndSchedule(t *testing.T) {
	s := newSQLiteStore(t)
	ensureMutationManySchema(t, s)
	runResolveMutationAndSchedule(t, s)
}
func runResolveMutationAndSchedule(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	prefix := fmt.Sprintf("reschedule-%d", now.UnixNano())
	a, b := prefix+"-a", prefix+"-b"
	if err := s.CreateConnection(ctx, Connection{ID: prefix, Provider: "google", AccountFingerprint: prefix, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, b} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: prefix, ProviderCalendarID: id, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_operation_members WHERE operation_id=?`), prefix+"-operation-012345")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_intents WHERE mutation_id=?`), ":"+prefix+"-operation-012345")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM projection_mutation_operations WHERE operation_id=?`), prefix+"-operation-012345")
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), prefix)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), prefix)
	})
	if err := s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Give the first calendar a live prior lease and cursors for both.
	for _, id := range []string{a, b} {
		if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`), "old", id); err != nil {
			t.Fatal(err)
		}
	}
	leaseUntil := now.Add(time.Hour)
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET status=?,generation=?,lease_owner=?,lease_until=? WHERE calendar_id=?`), "syncing", 5, "worker", leaseUntil, a); err != nil {
		t.Fatal(err)
	}
	old := CalendarSyncState{CalendarID: a, Generation: 5, Status: "syncing", LeaseOwner: "worker", LeaseUntil: &leaseUntil}
	op := prefix + "-operation-012345"
	if err := s.BeginProjectionMutationMany(ctx, []string{a, b}, op, now); err != nil {
		t.Fatal(err)
	}
	// If any calendar is ineligible, no cursor, lease, epoch or intent changes.
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET enabled=? WHERE id=?`), false, b); err != nil {
		t.Fatal(err)
	}
	before, err := s.InspectProjectionMutations(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, []string{a, b}, op, now); !errors.Is(err, ErrCalendarSyncIneligible) {
		t.Fatalf("ineligible resolution: %v", err)
	}
	after, err := s.InspectProjectionMutations(ctx, a)
	if err != nil || after != before || after.Pending != 1 {
		t.Fatalf("guard not rolled back: %+v %v", after, err)
	}
	var cursor, status, owner string
	var gen int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status,lease_owner,generation FROM calendar_sync_state WHERE calendar_id=?`), a).Scan(&cursor, &status, &owner, &gen); err != nil || cursor != "old" || status != "syncing" || owner != "worker" || gen != 5 {
		t.Fatalf("lease changed on rollback: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET enabled=? WHERE id=?`), true, b); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, []string{b, a}, op, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyEventSyncPage(ctx, old, EventSyncBatch{}, false, now); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("old worker remained valid: %v", err)
	}
	for _, id := range []string{a, b} {
		var c, st string
		var generation int
		if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status,generation FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&c, &st, &generation); err != nil || c != "" || st != "pending" || generation < 1 {
			t.Fatalf("replacement state: %v", err)
		}
		v, err := s.InspectProjectionMutations(ctx, id)
		if err != nil || v.Pending != 0 {
			t.Fatalf("pending intent: %+v %v", v, err)
		}
	}
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, []string{a, b}, op, now); !errors.Is(err, ErrProjectionMutationManyConflict) {
		t.Fatalf("replay: %v", err)
	}
}
