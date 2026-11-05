//go:build integration

package storage

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestPostgresRemovalStagingContract(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	ctx := t.Context()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := "removal-contract-" + strconv.FormatInt(now.UnixNano(), 10)
	if err = store.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), store.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = store.db.ExecContext(context.Background(), store.query(`DELETE FROM connections WHERE id=?`), id)
	})
	if err = store.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: id, ProviderCalendarID: id, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	leaseUntil := now.Add(time.Hour)
	if _, err = store.db.ExecContext(ctx, store.query(`UPDATE calendar_sync_state SET status=?,generation=?,lease_owner=?,lease_until=? WHERE calendar_id=?`), "syncing", 1, "worker", leaseUntil, id); err != nil {
		t.Fatal(err)
	}
	state := CalendarSyncState{CalendarID: id, Generation: 1, Status: "syncing", LeaseOwner: "worker", LeaseUntil: &leaseUntil}
	runRemovalStagingContract(t, store, state, now)
}
