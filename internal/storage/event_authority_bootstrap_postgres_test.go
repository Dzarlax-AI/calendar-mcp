//go:build integration

package storage

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestPostgresUnsuitableLegacyBootstrapDoesNotStarveNextAccount(t *testing.T) {
	store := newAuthorityPostgresStore(t, SchemaVersion)
	testUnsuitableLegacyBootstrap(t, store, bytes.Repeat([]byte{1}, 16385))
}

func TestPostgresLegacyBootstrapConcurrentCrashRecovery(t *testing.T) {
	store := newAuthorityPostgresStore(t, SchemaVersion)
	other := &Store{db: store.db, dialect: store.dialect}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if err := store.CreateConnection(ctx, Connection{ID: "legacy", Provider: "google", DisplayName: "Legacy", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	markConnectionMigrated(t, store, "legacy")
	if err := store.UpsertCalendar(ctx, Calendar{ID: "google:legacy:primary", ConnectionID: "legacy", ProviderCalendarID: "primary", Name: "Primary", CanRead: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan *CredentialSnapshot, 2)
	errs := make(chan error, 2)
	for _, candidate := range []*Store{store, other} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			snap, err := s.BeginOneLegacyGoogleAuthorization(ctx, now)
			results <- snap
			errs <- err
		}(candidate)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *CredentialSnapshot
	count := 0
	for snap := range results {
		if snap != nil {
			count++
			first = snap
		}
	}
	if count != 1 {
		t.Fatalf("claims=%d", count)
	}
	if again, err := other.BeginOneLegacyGoogleAuthorization(ctx, now.Add(time.Minute)); err != nil || again != nil {
		t.Fatalf("crash retried: %v %v", again, err)
	}
	recovery, err := other.BeginAuthorization(ctx, "legacy", []byte("replacement"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, *first, nil, now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("old completion=%v", err)
	}
	if err = other.CompleteGoogleAuthorization(ctx, recovery, nil, now); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresBeginFencesBlockedConsumedRepair(t *testing.T) {
	store := newAuthorityPostgresStore(t, SchemaVersion)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	id := "google:repair:primary"
	if err := store.CreateConnection(ctx, Connection{ID: "repair", Provider: "google", DisplayName: "Repair", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(ctx, "repair", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	entry := DiscoveredDetailCalendar{Calendar: Calendar{ID: id, ConnectionID: "repair", ProviderCalendarID: "primary", Name: "Primary"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	seed, err := store.ClaimDueCalendarSync(ctx, "seed", now, now.Add(time.Hour))
	if err != nil || seed == nil {
		t.Fatal(err)
	}
	next := now.Add(time.Minute)
	if err = store.ApplyEventSyncPage(ctx, *seed, EventSyncBatch{Warnings: []EventSyncWarning{{ObjectID: "broken", ETag: "etag", ErrorCode: "protocol"}}, NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ScheduleEventSyncObjectRepair(ctx, id, "broken", "etag", next); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimDueCalendarSync(ctx, "repair", next, next.Add(time.Hour))
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	ok, err := store.ConsumeEventSyncProviderMutationAuthorization(ctx, *claim, id, "broken", "etag", next)
	if err != nil || !ok {
		t.Fatalf("consume=%v %v", ok, err)
	}
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var pid int
	if err = blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var locked string
	if err = blocker.QueryRowContext(ctx, "SELECT calendar_id FROM calendar_sync_state WHERE calendar_id=$1 FOR UPDATE", id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	beginDone := make(chan error, 1)
	go func() { _, e := store.BeginAuthorization(ctx, "repair", nil, next); beginDone <- e }()
	beginPID := waitAuthorityBlockedBackend(t, ctx, store.db, pid)
	done := make(chan error, 1)
	go func() {
		event := calendar.EventV2{ID: "late", CalendarID: id, Provider: "google", Title: "old", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
		done <- store.ApplyEventSyncObjectRepair(ctx, *claim, EventSyncRepairBatch{ObjectID: "broken", ETag: "new", Outcome: calendar.EventSyncObjectProviderCorrected, Upserts: []CachedEventUpsert{{SourceObjectID: "broken", Event: event}}}, next)
	}()
	_ = waitAuthorityBlockedBackend(t, ctx, store.db, beginPID)
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-beginDone; err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("late repair=%v", err)
	}
	for _, table := range []string{"cached_events", "calendar_sync_objects", "calendar_sync_quarantine"} {
		var n int
		if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
}
