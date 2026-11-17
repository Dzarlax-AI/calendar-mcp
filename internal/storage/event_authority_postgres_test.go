//go:build integration

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"github.com/google/uuid"
)

// Each authority integration fixture has its own schema in the disposable
// local database. It cannot accidentally migrate an application database.
func newAuthorityPostgresStore(t *testing.T, version int) *Store {
	t.Helper()
	raw := os.Getenv("TEST_POSTGRES_URL")
	if raw == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || !strings.HasPrefix(u.Path, "/calendar_r6_") || u.User.Username() != "calendar_r6" {
		t.Fatal("authority integration requires a disposable loopback calendar_r6 database")
	}
	ctx := t.Context()
	admin, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "r6_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := admin.db.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		if err != nil {
			t.Errorf("remove disposable fixture schema: %v", err)
		}
		_ = admin.Close()
	})
	if version == SchemaVersion {
		if err = store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return store
	}
	for next := 1; next <= version; next++ {
		path := fmt.Sprintf("migrations/postgres/%03d_%s.sql", next, migrationName(next))
		script, readErr := migrations.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = store.db.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
		if _, err = store.db.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES($1,$2)", next, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestPostgresAuthorityMigration28To29(t *testing.T) {
	runAuthorityMigration28To29(t, newAuthorityPostgresStore(t, 28))
}

func TestPostgresAuthorityBeginFencesBlockedActualPage(t *testing.T) {
	store := newAuthorityPostgresStore(t, SchemaVersion)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const connectionID = "interleave"
	const calendarID = "google:interleave:primary"
	if err := store.CreateConnection(ctx, Connection{ID: connectionID, Provider: "google", DisplayName: "Synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.BeginAuthorization(ctx, connectionID, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, snapshot, []DiscoveredDetailCalendar{{Calendar: Calendar{ID: calendarID, ConnectionID: connectionID, ProviderCalendarID: "primary", Name: "Synthetic", GooglePrimary: true}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimDueCalendarSync(ctx, "old-worker", now, now.Add(time.Hour))
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	oldEvent := calendar.EventV2{ID: "old-private", CalendarID: calendarID, Provider: "google", Title: "OLD PRIVATE TITLE", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	if err = store.ApplyEventSyncPage(ctx, *claim, EventSyncBatch{Upserts: []CachedEventUpsert{{Event: oldEvent}}, FullSync: true}, false, now); err != nil {
		t.Fatal(err)
	}
	// This blocker holds only sync-state, making the real Begin stop after it
	// locks the connection. The page must then queue behind that connection,
	// rather than acquiring sync-state first and deadlocking with Begin.
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var blockerPID int
	if err = blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	var locked string
	if err = blocker.QueryRowContext(ctx, "SELECT calendar_id FROM calendar_sync_state WHERE calendar_id=$1 FOR UPDATE", calendarID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	beginDone := make(chan error, 1)
	go func() {
		_, err := store.BeginAuthorization(ctx, connectionID, nil, now.Add(time.Second))
		beginDone <- err
	}()
	beginPID := waitAuthorityBlockedBackend(t, ctx, store.db, blockerPID)
	pageDone := make(chan error, 1)
	go func() {
		event := calendar.EventV2{ID: "late-private", CalendarID: calendarID, Provider: "google", Title: "OLD PRIVATE TITLE", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
		pageDone <- store.ApplyEventSyncPage(ctx, *claim, EventSyncBatch{Upserts: []CachedEventUpsert{{Event: event}}, FullSync: true}, true, now.Add(2*time.Second))
	}()
	_ = waitAuthorityBlockedBackend(t, ctx, store.db, beginPID)
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-beginDone; err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if err = <-pageDone; !errors.Is(err, ErrCalendarSyncLeaseLost) && !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("late actual page error = %v", err)
	}
	var count int
	if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cached_events WHERE calendar_id=$1", calendarID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("late payload survived: rows=%d error=%v", count, err)
	}
}

func waitAuthorityBlockedBackend(t *testing.T, ctx context.Context, db *sql.DB, blockingPID int) int {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int
		err := db.QueryRowContext(ctx, "SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND datname=current_database() LIMIT 1", blockingPID).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no backend blocked behind pid %d: %v", blockingPID, ctx.Err())
		case <-tick.C:
		}
	}
}

func TestPostgresAuthorityParallelClaimsAndBeginEligibility(t *testing.T) {
	store := newAuthorityPostgresStore(t, SchemaVersion)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	const cid = "google:claim-account:primary"
	if err := store.CreateConnection(ctx, Connection{ID: "claim-account", Provider: "google", DisplayName: "Synthetic claim", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	grant := func() {
		t.Helper()
		snap, err := store.BeginAuthorization(ctx, "claim-account", nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.CompleteGoogleAuthorization(ctx, snap, []DiscoveredDetailCalendar{{Calendar: Calendar{ID: cid, ConnectionID: "claim-account", ProviderCalendarID: "primary", Name: "Synthetic"}, AccessRole: "owner"}}, now); err != nil {
			t.Fatal(err)
		}
		if err = store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	grant()
	start := make(chan struct{})
	results := make(chan *CalendarSyncState, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			state, err := store.ClaimDueCalendarSync(ctx, fmt.Sprintf("claim-%d", i), now, now.Add(time.Hour))
			results <- state
			failures <- err
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	close(failures)
	count := 0
	for state := range results {
		if state != nil {
			count++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("parallel claims=%d, want one", count)
	}
	grant()
	// Pause Begin after connection lock and before sync-state fencing. Claim's
	// initial candidate can see the old committed eligibility, but the actual
	// lock-time recheck must reject it after Begin commits.
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var blockerPID int
	var id string
	if err = blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if err = blocker.QueryRowContext(ctx, "SELECT calendar_id FROM calendar_sync_state WHERE calendar_id=$1 FOR UPDATE", cid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	beginDone := make(chan error, 1)
	go func() { _, err := store.BeginAuthorization(ctx, "claim-account", nil, now); beginDone <- err }()
	beginPID := waitAuthorityBlockedBackend(t, ctx, store.db, blockerPID)
	claimDone := make(chan *CalendarSyncState, 1)
	claimErr := make(chan error, 1)
	go func() {
		state, err := store.ClaimDueCalendarSync(ctx, "late-claim", now, now.Add(time.Hour))
		claimDone <- state
		claimErr <- err
	}()
	_ = waitAuthorityBlockedBackend(t, ctx, store.db, beginPID)
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-beginDone; err != nil {
		t.Fatal(err)
	}
	if state := <-claimDone; state != nil {
		t.Fatalf("claimed revoked source: %+v", state)
	}
	if err = <-claimErr; err != nil {
		t.Fatal(err)
	}
}
