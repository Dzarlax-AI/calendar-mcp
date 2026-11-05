package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func ensureMutationManySchema(t *testing.T, s *Store) {
	t.Helper()
	var count int
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projection_mutation_operations'`
	path := "migrations/sqlite/018_projection_mutation_many.sql"
	if s.dialect == DialectPostgres {
		q = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='projection_mutation_operations'`
		path = "migrations/postgres/018_projection_mutation_many.sql"
	}
	if err := s.db.QueryRow(q).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		script, err := migrations.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(string(script)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectionMutationManySQLite(t *testing.T) {
	s := newSQLiteStore(t)
	ensureMutationManySchema(t, s)
	runProjectionMutationManyContract(t, s, s, fmt.Sprintf("many-%d", time.Now().UnixNano()))
}

func runProjectionMutationManyContract(t *testing.T, first, second *Store, prefix string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	if err := first.CreateConnection(ctx, Connection{ID: prefix, Provider: "google", AccountFingerprint: prefix, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	a, b := prefix+"-a", prefix+"-b"
	for _, id := range []string{a, b} {
		if err := first.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: prefix, ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = first.db.ExecContext(context.Background(), first.query(`DELETE FROM projection_mutation_operations WHERE operation_id LIKE ?`), prefix+"%")
		_, _ = first.db.ExecContext(context.Background(), first.query(`DELETE FROM calendars WHERE connection_id=?`), prefix)
		_, _ = first.db.ExecContext(context.Background(), first.query(`DELETE FROM connections WHERE id=?`), prefix)
	})
	missing := prefix + "-missing"
	if err := first.BeginProjectionMutationMany(ctx, []string{a, missing}, prefix+"-rollback-012345", now); err == nil {
		t.Fatal("missing calendar accepted")
	}
	if _, err := first.InspectProjectionMutations(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial guard on rollback: %v", err)
	}
	// The single API retains its existing ID domain and can never collide with
	// the internal colon-prefixed members of a multi-calendar operation.
	sharedID := prefix + "-shared-0123456789"
	sharedErrors := make(chan error, 2)
	go func() { sharedErrors <- first.beginProjectionMutation(ctx, a, sharedID, now) }()
	go func() { sharedErrors <- second.BeginProjectionMutationMany(ctx, []string{b, a}, sharedID, now) }()
	for i := 0; i < 2; i++ {
		if err := <-sharedErrors; err != nil {
			t.Fatalf("single/many same ID: %v", err)
		}
	}
	if err := first.resolveProjectionMutation(ctx, a, sharedID); err != nil {
		t.Fatalf("single resolution: %v", err)
	}
	if err := first.ResolveProjectionMutationManyAndSchedule(ctx, []string{a, b}, sharedID, now); err != nil {
		t.Fatalf("many resolution: %v", err)
	}
	baselines := make(map[string]int64)
	for _, id := range []string{a, b} {
		v, err := first.InspectProjectionMutations(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		baselines[id] = v.Epoch
	}
	op := prefix + "-first-0123456789"
	if err := first.BeginProjectionMutationMany(ctx, []string{b, a, a}, op, now); err != nil {
		t.Fatal(err)
	}
	observed, err := first.GetProjectionMutationMany(ctx, op)
	if err != nil || observed.ResolvedAt != nil || len(observed.CalendarIDs) != 2 || observed.CalendarIDs[0] != a || observed.CalendarIDs[1] != b {
		t.Fatalf("lookup pending: %+v %v", observed, err)
	}
	if err := first.BeginProjectionMutationMany(ctx, []string{a, b}, op, now); err == nil {
		t.Fatal("duplicate operation accepted")
	}
	if err := first.resolveProjectionMutation(ctx, a, op); err == nil {
		t.Fatal("single resolution bypassed multi-calendar operation")
	}
	if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM projection_mutation_intents WHERE calendar_id=? AND mutation_id=?`), a, ":"+op); err == nil {
		t.Fatal("foreign key permitted a partial multi-calendar deletion")
	}
	if err := first.ResolveProjectionMutationManyAndSchedule(ctx, []string{a}, op, now); !errors.Is(err, ErrProjectionMutationManyConflict) {
		t.Fatalf("partial resolve: %v", err)
	}
	for _, id := range []string{a, b} {
		state, err := first.InspectProjectionMutations(ctx, id)
		if err != nil || state.Pending != 1 || state.Epoch != baselines[id]+1 {
			t.Fatalf("state: %+v %v", state, err)
		}
	}
	if err := first.ResolveProjectionMutationManyAndSchedule(ctx, []string{a, b}, op, now); err != nil {
		t.Fatal(err)
	}
	observed, err = first.GetProjectionMutationMany(ctx, op)
	if err != nil || observed.ResolvedAt == nil {
		t.Fatalf("lookup resolved: %+v %v", observed, err)
	}
	if err := first.ResolveProjectionMutationManyAndSchedule(ctx, []string{b, a}, op, now); !errors.Is(err, ErrProjectionMutationManyConflict) {
		t.Fatalf("repeat resolution: %v", err)
	}
	if err := first.BeginProjectionMutationMany(ctx, []string{a, b}, op, now); err == nil {
		t.Fatal("resolved ID reused")
	}
	errs := make(chan error, 2)
	go func() {
		errs <- first.BeginProjectionMutationMany(ctx, []string{a, b}, prefix+"-parallel1-012345", now)
	}()
	go func() {
		errs <- second.BeginProjectionMutationMany(ctx, []string{b, a}, prefix+"-parallel2-012345", now)
	}()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("opposite-order begin: %v", err)
		}
	}
	for _, id := range []string{a, b} {
		state, err := first.InspectProjectionMutations(ctx, id)
		if err != nil || state.Pending != 2 || state.Epoch != baselines[id]+4 {
			t.Fatalf("parallel state: %+v %v", state, err)
		}
	}
	for _, operation := range []string{prefix + "-parallel1-012345", prefix + "-parallel2-012345"} {
		if err := first.ResolveProjectionMutationManyAndSchedule(ctx, []string{b, a}, operation, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectionMutationManyPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "calendar.db")
	url := "sqlite://" + path
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureMutationManySchema(t, s)
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, Connection{ID: "reopen", Provider: "google", AccountFingerprint: "reopen", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"reopen-a", "reopen-b"} {
		if err = s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "reopen", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ids := []string{"reopen-a", "reopen-b"}
	op := "reopen-operation-0123456789"
	if err = s.BeginProjectionMutationMany(ctx, ids, op, now); err != nil {
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
	for _, id := range ids {
		state, err := s.InspectProjectionMutations(ctx, id)
		if err != nil || state.Pending != 1 {
			t.Fatalf("pending after reopen: %+v %v", state, err)
		}
	}
	if err = s.BeginProjectionMutationMany(ctx, ids, op, now); err == nil {
		t.Fatal("duplicate after reopen")
	}
	if err = s.ResolveProjectionMutationManyAndSchedule(ctx, ids, op, now); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionMutationManyRollsBackLateInsertFailure(t *testing.T) {
	s := newSQLiteStore(t)
	ensureMutationManySchema(t, s)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.CreateConnection(ctx, Connection{ID: "fault-account", Provider: "google", AccountFingerprint: "fault-account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fault-a", "fault-b"} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "fault-account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_second_mutation BEFORE INSERT ON projection_mutation_operation_members WHEN NEW.calendar_id='fault-b' BEGIN SELECT RAISE(ABORT, 'synthetic'); END`); err != nil {
		t.Fatal(err)
	}
	op := "fault-operation-01234567"
	if err := s.BeginProjectionMutationMany(ctx, []string{"fault-b", "fault-a"}, op, now); err == nil {
		t.Fatal("injected failure not observed")
	}
	for _, id := range []string{"fault-a", "fault-b"} {
		if _, err := s.InspectProjectionMutations(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("partial guard %s: %v", id, err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projection_mutation_operations WHERE operation_id=?`, op).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial operation count=%d err=%v", count, err)
	}
}
