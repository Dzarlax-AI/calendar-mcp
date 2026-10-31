//go:build integration

package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPostgresAppleAuthRecovery(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	store, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.CreateConnection(t.Context(), Connection{ID: "auth-recovery-connection", Provider: "apple", AccountFingerprint: "synthetic-auth-recovery", DisplayName: "Synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(t.Context(), Calendar{ID: "auth-recovery-calendar", ConnectionID: "auth-recovery-connection", ProviderCalendarID: "synthetic", Name: "Synthetic", CanRead: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := Open(context.Background(), databaseURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.db.Exec("DELETE FROM calendars WHERE id='auth-recovery-calendar'")
		_, _ = cleanup.db.Exec("DELETE FROM connections WHERE id='auth-recovery-connection'")
	})
	window := SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	if err := store.EnsureCalendarSyncStates(t.Context(), now, window); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(t.Context(), "first", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != "auth-recovery-calendar" {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := store.ParkCalendarSync(t.Context(), *state, "auth", now); err != nil {
			t.Fatal(err)
		}
		// A UTC window rollover must preserve the parked error and deadline.
		window.End = window.End.Add(time.Hour)
		if err := store.EnsureCalendarSyncStates(t.Context(), now, window); err != nil {
			t.Fatal(err)
		}
		early, err := store.ClaimDueCalendarSync(t.Context(), "early", now.Add(14*time.Minute), now.Add(time.Hour))
		if err != nil || early != nil {
			t.Fatalf("early=%#v err=%v", early, err)
		}
		now = now.Add(15 * time.Minute)
		state, err = store.ClaimDueCalendarSync(t.Context(), "retry", now, now.Add(time.Hour))
		if err != nil || state == nil {
			t.Fatalf("retry=%#v err=%v", state, err)
		}
	}
	next := now.Add(5 * time.Minute)
	if err := store.ApplyEventSyncPage(t.Context(), *state, EventSyncBatch{NextCursor: "recovered", NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	var status, cursor string
	if err := store.db.QueryRow("SELECT status,cursor FROM calendar_sync_state WHERE calendar_id='auth-recovery-calendar'").Scan(&status, &cursor); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || cursor != "recovered" {
		t.Fatalf("status=%s cursor=%s", status, cursor)
	}
}
