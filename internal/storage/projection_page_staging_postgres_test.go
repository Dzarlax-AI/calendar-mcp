//go:build integration

package storage

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestPostgresProjectionPageStaging(t *testing.T) {
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
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureProjectionPageStagingSchema(t, s)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := "projection-page-" + strconv.FormatInt(now.UnixNano(), 10)
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: id, ProviderCalendarID: "synthetic", Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	runProjectionPageStagingContract(t, s, id, now)
}

func TestPostgresProjectionPageVerifiedIncrementalBase(t *testing.T) {
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
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := "projection-verified-base-" + strconv.FormatInt(now.UnixNano(), 10)
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: id, ProviderCalendarID: "synthetic", Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	runProjectionPageMatchingIncrementalBase(t, s, id, now)
}

// This reproduces the target-intent lock order without making any provider
// call: target owns calendar and wants guard while staging starts. Staging must
// wait on calendar before acquiring guard, so target can finish without a cycle.
func TestPostgresProjectionPageLockOrder(t *testing.T) {
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
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureProjectionPageStagingSchema(t, s)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := "projection-lock-order-" + strconv.FormatInt(now.UnixNano(), 10)
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: id, ProviderCalendarID: "synthetic", Name: "synthetic", Timezone: "Europe/Belgrade", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "page-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0)`), id); err != nil {
		t.Fatal(err)
	}
	target, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Rollback() }()
	var locked string
	if err := target.QueryRowContext(ctx, s.query(`SELECT id FROM calendars WHERE id=? FOR UPDATE`), id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	attempt := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "lock-order-0123456789", Timezone: "Europe/Belgrade", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	result := make(chan error, 1)
	go func() { result <- s.StartProjectionPageAttempt(ctx, *state, attempt, now) }()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT connection_id FROM calendars WHERE id=%FOR NO KEY UPDATE%')`).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("staging did not wait on the calendar lock")
	}
	guardCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := lockProjectionGuardRow(guardCtx, s, target, id); err != nil {
		t.Fatalf("guard blocked behind staging: %v", err)
	}
	if err := target.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("staging failed to complete after lock release")
	}

	// A calendar NO KEY UPDATE lock must allow the current page path's
	// cached_events foreign-key KEY SHARE while it owns the sync lease.
	compatible, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compatible.Rollback() }()
	if err := compatible.QueryRowContext(ctx, s.query(`SELECT id FROM calendars WHERE id=? FOR NO KEY UPDATE`), id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	applyCtx, applyCancel := context.WithTimeout(ctx, 2*time.Second)
	applied := EventSyncBatch{Upserts: []CachedEventUpsert{{Event: calendar.EventV2{ID: "synthetic-event", CalendarID: id, Provider: "google", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}}}}
	applyErr := s.ApplyEventSyncPage(applyCtx, *state, applied, false, now)
	applyCancel()
	if applyErr != nil {
		t.Fatalf("cached event FK conflicted with calendar lock: %v", applyErr)
	}
	if err := compatible.Commit(); err != nil {
		t.Fatal(err)
	}

	// A lease which expires while waiting for the same calendar lock must not
	// be accepted using only the caller's pre-wait clock sample.
	sample := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET lease_until=? WHERE calendar_id=?`), sample.Add(200*time.Millisecond), id); err != nil {
		t.Fatal(err)
	}
	holder, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if err := holder.QueryRowContext(ctx, s.query(`SELECT id FROM calendars WHERE id=? FOR UPDATE`), id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	page := calendar.EventSyncPage{SourceTimeZone: "Europe/Belgrade", Complete: true, NextCursor: "synthetic-cursor"}
	expired := make(chan error, 1)
	go func() {
		expired <- s.AppendProjectionPage(ctx, *state, attempt.AttemptID, 0, "", "Europe/Belgrade", page, sample)
	}()
	time.Sleep(350 * time.Millisecond)
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-expired:
		if !errors.Is(err, ErrCalendarSyncLeaseLost) {
			t.Fatalf("expired waiting lease: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired staging did not return")
	}
}
