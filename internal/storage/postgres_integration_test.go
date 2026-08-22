//go:build integration

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestPostgresStorageContract(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	store, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runStorageContract(t, store)
	testPostgresClaimsOnlyOneJobPerRule(t, databaseURL, store)
	testPostgresFinishAndRecoveryUseConsistentLockOrder(t, databaseURL, store)
}

func TestPostgresEventReadModelMalformedObjectParityFixture(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	connectionID := "parity-fixture-connection-" + suffix
	calendarID := "parity-fixture-calendar-" + suffix
	now := time.Now().UTC().Truncate(time.Microsecond)
	window := SyncWindow{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = store.db.ExecContext(cleanupCtx, store.query("DELETE FROM calendars WHERE id=?"), calendarID)
		_, _ = store.db.ExecContext(cleanupCtx, store.query("DELETE FROM connections WHERE id=?"), connectionID)
	})

	if err := store.CreateConnection(ctx, Connection{
		ID: connectionID, Provider: "apple", DisplayName: "Synthetic parity fixture", Status: "connected",
		EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(ctx, Calendar{
		ID: calendarID, ConnectionID: connectionID, ProviderCalendarID: "synthetic-calendar",
		Name: "Synthetic parity fixture", CanRead: true, DiscoveredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}

	event := func(id, objectID, title string) CachedEventUpsert {
		return CachedEventUpsert{SourceObjectID: objectID, Event: calendar.EventV2{
			ID: id, CalendarID: calendarID, Provider: "apple", Title: title,
			Start: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
			End:   calendar.EventTime{DateTime: now.Add(3 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		}}
	}
	objects := func() []SyncObject {
		return []SyncObject{{ObjectID: "valid-a.ics", ETag: "a-v1"}, {ObjectID: "valid-b.ics", ETag: "b-v1"}, {ObjectID: "malformed.ics", ETag: "m-v1"}}
	}

	initial, err := store.ClaimDueCalendarSync(ctx, "parity-initial-worker", now, now.Add(time.Hour))
	if err != nil || initial == nil || initial.CalendarID != calendarID {
		t.Fatalf("initial claim=%#v err=%v", initial, err)
	}
	initialSuccess := now.Add(time.Minute)
	if err := store.ApplyEventSyncPage(ctx, *initial, EventSyncBatch{
		FullSync: true,
		Upserts: []CachedEventUpsert{
			event("valid-a", "valid-a.ics", "valid A"),
			event("valid-b", "valid-b.ics", "valid B"),
			// This is the prior membership of an object that becomes unresolved
			// on the next provider attempt; its payload is entirely synthetic.
			event("old-malformed", "malformed.ics", "old unresolved membership"),
		},
		Objects: objects(), NextCursor: "cursor-before-malformed",
		NextSyncAt: func() *time.Time { value := now.Add(2 * time.Hour); return &value }(),
	}, true, initialSuccess); err != nil {
		t.Fatal(err)
	}

	var beforeCursor, beforeStatus string
	var beforeCode sql.NullString
	var beforeGeneration int64
	var beforeSuccess time.Time
	if err := store.db.QueryRowContext(ctx, store.query("SELECT cursor, status, generation, last_success_at, last_error_code FROM calendar_sync_state WHERE calendar_id=?"), calendarID).Scan(&beforeCursor, &beforeStatus, &beforeGeneration, &beforeSuccess, &beforeCode); err != nil {
		t.Fatal(err)
	}
	if beforeCursor != "cursor-before-malformed" || beforeStatus != "ready" || beforeCode.Valid {
		t.Fatalf("initial state cursor=%q status=%q code=%q", beforeCursor, beforeStatus, beforeCode.String)
	}
	var oldObjectGeneration int64
	if err := store.db.QueryRowContext(ctx, store.query("SELECT sync_generation FROM calendar_sync_objects WHERE calendar_id=? AND object_id=?"), calendarID, "malformed.ics").Scan(&oldObjectGeneration); err != nil {
		t.Fatal(err)
	}

	degradedAt := now.Add(2 * time.Hour)
	degraded, err := store.ClaimDueCalendarSync(ctx, "parity-degraded-worker", degradedAt, degradedAt.Add(time.Hour))
	if err != nil || degraded == nil || degraded.CalendarID != calendarID {
		t.Fatalf("degraded claim=%#v err=%v", degraded, err)
	}
	if degraded.Cursor != beforeCursor {
		t.Fatalf("degraded claim cursor=%q, want authoritative %q", degraded.Cursor, beforeCursor)
	}
	degradedRetry := degradedAt.Add(5 * time.Minute)
	if err := store.ApplyEventSyncPage(ctx, *degraded, EventSyncBatch{
		FullSync: true, Degraded: true, ErrorCode: "protocol",
		Upserts: []CachedEventUpsert{event("valid-a", "valid-a.ics", "valid A refreshed"), event("valid-b", "valid-b.ics", "valid B refreshed")},
		Objects: []SyncObject{{ObjectID: "valid-a.ics", ETag: "a-v2"}, {ObjectID: "valid-b.ics", ETag: "b-v2"}},
		// A provider cursor returned alongside a malformed object is not
		// authoritative and must not replace the last successful cursor.
		NextCursor: "must-not-advance-on-malformed-object", NextSyncAt: &degradedRetry,
	}, true, degradedAt); err != nil {
		t.Fatal(err)
	}

	events, statuses, err := store.ListCachedEvents(ctx, []string{calendarID}, window.Start, window.End)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("degraded events=%#v, want two valid plus unresolved old membership", events)
	}
	seen := make(map[string]calendar.EventV2, len(events))
	for _, value := range events {
		seen[value.ID] = value
	}
	if seen["valid-a"].Title != "valid A refreshed" || seen["valid-b"].Title != "valid B refreshed" || seen["old-malformed"].Title != "old unresolved membership" {
		t.Fatalf("degraded projection=%#v", seen)
	}
	if len(statuses) != 1 || statuses[0].Status != "degraded" || statuses[0].ErrorCode != "protocol" || statuses[0].LastSuccessAt == nil || !statuses[0].LastSuccessAt.Equal(beforeSuccess) {
		t.Fatalf("degraded source status=%#v", statuses)
	}

	var cursor, status string
	var code sql.NullString
	var generation int64
	var success time.Time
	var leaseOwner sql.NullString
	var leaseUntil sql.NullTime
	if err := store.db.QueryRowContext(ctx, store.query("SELECT cursor, status, generation, last_success_at, last_error_code, lease_owner, lease_until FROM calendar_sync_state WHERE calendar_id=?"), calendarID).Scan(&cursor, &status, &generation, &success, &code, &leaseOwner, &leaseUntil); err != nil {
		t.Fatal(err)
	}
	if cursor != beforeCursor || generation != degraded.Generation || !success.Equal(beforeSuccess) || status != "degraded" || !code.Valid || code.String != "protocol" || leaseOwner.Valid || leaseUntil.Valid {
		t.Fatalf("degraded state cursor=%q generation=%d success=%s status=%q code=%q lease=%q/%v", cursor, generation, success, status, code.String, leaseOwner.String, leaseUntil)
	}
	var afterObjectGeneration int64
	if err := store.db.QueryRowContext(ctx, store.query("SELECT sync_generation FROM calendar_sync_objects WHERE calendar_id=? AND object_id=?"), calendarID, "malformed.ics").Scan(&afterObjectGeneration); err != nil {
		t.Fatal(err)
	}
	if afterObjectGeneration != oldObjectGeneration {
		t.Fatalf("unresolved object generation=%d, want %d", afterObjectGeneration, oldObjectGeneration)
	}

	repairAt := degradedRetry
	repair, err := store.ClaimDueCalendarSync(ctx, "parity-repair-worker", repairAt, repairAt.Add(time.Hour))
	if err != nil || repair == nil || repair.CalendarID != calendarID {
		t.Fatalf("repair claim=%#v err=%v", repair, err)
	}
	if repair.Cursor != beforeCursor {
		t.Fatalf("repair replay cursor=%q, want unchanged authoritative %q", repair.Cursor, beforeCursor)
	}
	repairedSuccess := repairAt.Add(time.Minute)
	if err := store.ApplyEventSyncPage(ctx, *repair, EventSyncBatch{
		FullSync: true,
		Upserts:  []CachedEventUpsert{event("valid-a", "valid-a.ics", "valid A repaired"), event("valid-b", "valid-b.ics", "valid B repaired"), event("repaired", "malformed.ics", "malformed object repaired")},
		Objects:  objects(), ReplacedObjectIDs: []string{"valid-a.ics", "valid-b.ics", "malformed.ics"}, NextCursor: "cursor-after-repair",
	}, true, repairedSuccess); err != nil {
		t.Fatal(err)
	}

	if err := store.db.QueryRowContext(ctx, store.query("SELECT cursor, status, generation, last_success_at, last_error_code FROM calendar_sync_state WHERE calendar_id=?"), calendarID).Scan(&cursor, &status, &generation, &success, &code); err != nil {
		t.Fatal(err)
	}
	if cursor != "cursor-after-repair" || status != "ready" || generation != repair.Generation || !success.Equal(repairedSuccess) || code.Valid {
		t.Fatalf("repaired state cursor=%q status=%q generation=%d success=%s code=%q", cursor, status, generation, success, code.String)
	}
	events, _, err = store.ListCachedEvents(ctx, []string{calendarID}, window.Start, window.End)
	if err != nil || len(events) != 3 {
		t.Fatalf("repaired events=%#v err=%v", events, err)
	}
	seen = make(map[string]calendar.EventV2, len(events))
	for _, value := range events {
		seen[value.ID] = value
	}
	if seen["valid-a"].Title != "valid A repaired" || seen["valid-b"].Title != "valid B repaired" || seen["repaired"].Title != "malformed object repaired" || seen["old-malformed"].ID != "" {
		t.Fatalf("repaired projection=%#v", seen)
	}
}

func testPostgresClaimsOnlyOneJobPerRule(t *testing.T, databaseURL string, first *Store) {
	t.Helper()
	ctx := context.Background()
	second, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	now := time.Now().UTC()
	rule := Rule{ID: "parallel-claim-rule", SourceCalendarID: "microsoft:ms:source-provider", TargetCalendarID: "google:google:target-provider", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := first.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	for iteration := 0; iteration < 20; iteration++ {
		ids := []string{fmt.Sprintf("parallel-a-%d", iteration), fmt.Sprintf("parallel-b-%d", iteration)}
		for _, id := range ids {
			if err := first.EnqueueJob(ctx, Job{ID: id, RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		results := make(chan *Job, 2)
		errorsFound := make(chan error, 2)
		var workers sync.WaitGroup
		for workerNumber, candidateStore := range []*Store{first, second} {
			workers.Add(1)
			go func(worker int, candidate *Store) {
				defer workers.Done()
				<-start
				job, claimErr := candidate.ClaimJob(ctx, fmt.Sprintf("worker-%d", worker), now)
				if claimErr != nil {
					errorsFound <- claimErr
					return
				}
				results <- job
			}(workerNumber, candidateStore)
		}
		close(start)
		workers.Wait()
		close(results)
		close(errorsFound)
		for claimErr := range errorsFound {
			t.Fatal(claimErr)
		}
		claimed := 0
		for job := range results {
			if job != nil {
				claimed++
			}
		}
		if claimed != 1 {
			t.Fatalf("iteration %d claimed %d jobs for one rule, want 1", iteration, claimed)
		}
		if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM sync_jobs WHERE id IN (?, ?)`), ids[0], ids[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func testPostgresFinishAndRecoveryUseConsistentLockOrder(t *testing.T, databaseURL string, first *Store) {
	t.Helper()
	ctx := context.Background()
	second, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	now := time.Now().UTC()
	rule := Rule{ID: "finish-recovery-rule", SourceCalendarID: "microsoft:ms:source-provider", TargetCalendarID: "google:google:target-provider", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := first.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	for iteration := 0; iteration < 20; iteration++ {
		jobID := fmt.Sprintf("finish-recovery-job-%d", iteration)
		runID := fmt.Sprintf("finish-recovery-run-%d", iteration)
		if err := first.EnqueueJob(ctx, Job{ID: jobID, RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		claimed, err := first.ClaimJob(ctx, "race-worker", now)
		if err != nil || claimed == nil || claimed.ID != jobID {
			t.Fatalf("iteration %d claim = %#v, err = %v", iteration, claimed, err)
		}
		if err := first.StartRun(ctx, Run{ID: runID, JobID: jobID, RuleID: rule.ID, Trigger: "manual", Outcome: "running", StartedAt: now}); err != nil {
			t.Fatal(err)
		}

		runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		start := make(chan struct{})
		finishDone := make(chan error, 1)
		recoverDone := make(chan struct {
			count int
			err   error
		}, 1)
		go func(job Job) {
			<-start
			finishDone <- first.FinishRun(runCtx, runID, job, "succeeded", now.Add(2*time.Minute), Run{})
		}(*claimed)
		go func() {
			<-start
			count, recoverErr := second.RecoverStaleJobs(runCtx, now.Add(time.Minute), now.Add(2*time.Minute))
			recoverDone <- struct {
				count int
				err   error
			}{count: count, err: recoverErr}
		}()
		close(start)
		finishErr := <-finishDone
		recovery := <-recoverDone
		cancel()
		if recovery.err != nil {
			t.Fatalf("iteration %d recovery error: %v", iteration, recovery.err)
		}
		if finishErr == nil && recovery.count != 0 {
			t.Fatalf("iteration %d finished job was also recovered", iteration)
		}
		if errors.Is(finishErr, ErrJobLeaseLost) && recovery.count != 1 {
			t.Fatalf("iteration %d lost lease without recovery", iteration)
		}
		if finishErr != nil && !errors.Is(finishErr, ErrJobLeaseLost) {
			t.Fatalf("iteration %d finish error: %v", iteration, finishErr)
		}
		if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM sync_runs WHERE id=?`), runID); err != nil {
			t.Fatal(err)
		}
		if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM sync_jobs WHERE id=?`), jobID); err != nil {
			t.Fatal(err)
		}
	}
}
