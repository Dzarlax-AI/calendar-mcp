//go:build integration

package storage

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestPostgresProjectionAttemptCleanup(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	ctx := t.Context()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureProjectionPageStagingSchema(t, s)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := "projection-cleanup-" + strconv.FormatInt(now.UnixNano(), 10)
	if err = s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	if err = s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: id, ProviderCalendarID: "synthetic", Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	runProjectionAttemptCleanup(t, s, id, now)
}
