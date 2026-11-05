package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestSourceProjectionTerminalPageSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runSourceProjectionTerminalPage(t, s, "calendar", now)
}
func runSourceProjectionTerminalPage(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "UTC", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "terminal-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	in := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "terminal-attempt-0123456789", Timezone: "UTC", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	if err = s.StartProjectionPageAttempt(ctx, *state, in, now); err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "event", CalendarID: id, Provider: "google", Start: calendar.EventTime{DateTime: now.Add(-time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	first := calendar.EventSyncPage{SourceTimeZone: "UTC", NextPageToken: "next", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: event.ID}, Event: event}}}
	if err = s.ApplyProjectionStagedPage(ctx, *state, in.AttemptID, 0, "", "UTC", first, now); err != nil {
		t.Fatal(err)
	}
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "cursor", DeletedEventIDs: []string{"cancelled"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "cancelled", Reason: calendar.EventSyncProviderCancellation}}}
	// A stale lock time fails before committing even the final page.
	if _, err = s.finalizeSourceProjectionPage(ctx, *state, in.AttemptID, 1, "next", "UTC", page, now.Add(2*time.Hour), now.Add(3*time.Hour)); err == nil {
		t.Fatal("expired lease accepted")
	}
	var count int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_page_attempt_pages WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("terminal page survived rollback: %d %v", count, err)
	}
	var connection string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT connection_id FROM calendars WHERE id=?`), id).Scan(&connection); err != nil {
		t.Fatal(err)
	}
	target := id + "-reservation-target"
	if err = s.UpsertCalendar(ctx, Calendar{ID: target, ConnectionID: connection, ProviderCalendarID: target, Name: target, Timezone: "UTC", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	reservationID := id + "-source-publication-reservation"
	publicationID := id + "-publication-held"
	concurrentID := id + "-concurrent-run"
	activeRunID := id + "-active-copy-run"
	racingRunID := id + "-racing-copy-run"
	// These identifiers and any failed/parked reservation belong only to this
	// fixture. Clean them even after t.Fatal so repeated PostgreSQL runs cannot
	// retain the shared global key or immutable operation IDs.
	t.Cleanup(func() {
		ctx := context.Background()
		for _, operation := range []string{reservationID, publicationID, concurrentID, activeRunID, racingRunID} {
			_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM exclusive_dispatch_guard_members WHERE operation_id=?`), operation)
			_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM exclusive_dispatch_reservations WHERE operation_id=?`), operation)
			_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operation_members WHERE operation_id=?`), operation)
			_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_intents WHERE mutation_id=?`), ":"+operation)
			_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operations WHERE operation_id=?`), operation)
		}
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM sync_rules WHERE id=?`), id+"-copy-rule")
	})
	if err = s.BeginExclusiveDispatch(ctx, ExclusiveDispatchRequest{OperationID: reservationID, TargetKey: GlobalSyncDispatchKey, RequestHash: projectionPageHash([]byte("synthetic")), Kind: "run", CalendarIDs: []string{target}, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.finalizeSourceProjectionPage(ctx, *state, in.AttemptID, 1, "next", "UTC", page, now, now.Add(time.Hour)); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("active reservation did not block publication: %v", err)
	}
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_page_attempt_pages WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reservation conflict lost staged page: %d %v", count, err)
	}
	var beforeCursor, beforeStatus string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&beforeCursor, &beforeStatus); err != nil || beforeCursor != "" || beforeStatus != "syncing" {
		t.Fatalf("reservation conflict changed cursor: %v", err)
	}
	if err = s.ReleaseExclusiveAfterConfirmedRun(ctx, reservationID, []string{target}, now); err != nil {
		t.Fatal(err)
	}
	// Model the source publication transaction holding the global key while
	// another worker begins. Its acquisition must wait until publication has
	// committed, even when the source deletes its temporary row before commit.
	holder, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, err := normalizeMutationCalendars([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operations(operation_id,calendar_ids,started_at) VALUES(?,?,?)`), publicationID, encoded, now); err != nil {
		_ = holder.Rollback()
		t.Fatal(err)
	}
	if err = s.insertExclusiveReservation(ctx, holder, ExclusiveDispatchRequest{OperationID: publicationID, TargetKey: GlobalSyncDispatchKey, RequestHash: projectionPageHash([]byte("publication")), Kind: "run", Now: now}); err != nil {
		_ = holder.Rollback()
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- s.BeginExclusiveDispatch(ctx, ExclusiveDispatchRequest{OperationID: concurrentID, TargetKey: GlobalSyncDispatchKey, RequestHash: projectionPageHash([]byte("run")), Kind: "run", CalendarIDs: []string{target}, Now: now})
	}()
	<-started
	select {
	case err = <-result:
		_ = holder.Rollback()
		t.Fatalf("run crossed uncommitted publication: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err = holder.ExecContext(ctx, s.query(`DELETE FROM exclusive_dispatch_reservations WHERE operation_id=?`), publicationID); err != nil {
		_ = holder.Rollback()
		t.Fatal(err)
	}
	if _, err = holder.ExecContext(ctx, s.query(`DELETE FROM projection_mutation_operations WHERE operation_id=?`), publicationID); err != nil {
		_ = holder.Rollback()
		t.Fatal(err)
	}
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatalf("run did not acquire after publication commit: %v", err)
	}
	if err = s.ReleaseExclusiveAfterConfirmedRun(ctx, concurrentID, []string{target}, now); err != nil {
		t.Fatal(err)
	}
	revision, err := s.finalizeSourceProjectionPage(ctx, *state, in.AttemptID, 1, "next", "UTC", page, now, now.Add(time.Hour))
	if err != nil || revision != 1 {
		t.Fatalf("finalize: %d %v", revision, err)
	}
	var cursor, status string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&cursor, &status); err != nil || cursor != "cursor" || status != "ready" {
		t.Fatalf("sync state: %v", err)
	}
	snap, err := s.GetProjectionSnapshot(ctx, id, revision)
	if err != nil || !snap.Verified || snap.Cursor != "cursor" {
		t.Fatalf("snapshot: %v", err)
	}
	var provenance []byte
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT observations FROM projection_revision_provenance WHERE calendar_id=? AND revision=?`), id, revision).Scan(&provenance); err != nil || len(provenance) == 0 {
		t.Fatalf("provenance: %v", err)
	}
	readAt := now.Add(time.Second)
	projection, err := s.GetSourceProjectionForCopy(ctx, id, state.WindowStart, state.WindowEnd, readAt, time.Minute)
	if err != nil || len(projection.Events) != 1 || len(projection.Observations) != 1 || projection.Snapshot.Revision != revision {
		t.Fatalf("atomic reader: %v", err)
	}
	if err = s.CheckSourceProjectionCurrent(ctx, id, revision+1, state.WindowStart, state.WindowEnd, readAt, time.Minute); err == nil {
		t.Fatal("wrong revision accepted")
	}
	// A cache Run reserves the global key. It prevents the background worker
	// from claiming/rebasing this source, while ordinary reads remain valid.
	if err = s.CreateRule(ctx, Rule{ID: id + "-copy-rule", SourceCalendarID: id, TargetCalendarID: target, State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	activeID := activeRunID
	if err = s.BeginExclusiveDispatch(ctx, ExclusiveDispatchRequest{OperationID: activeID, TargetKey: GlobalSyncDispatchKey, RequestHash: projectionPageHash([]byte("active")), Kind: "run", CalendarIDs: []string{target}, Now: now}); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Hour)
	claimed, err := s.ClaimDueCalendarSync(ctx, "blocked-source", later, later.Add(time.Hour))
	if err != nil || claimed != nil {
		t.Fatalf("source claimed during run: %v", err)
	}
	newWindow := SyncWindow{Start: state.WindowStart.AddDate(0, 0, 1), End: state.WindowEnd.AddDate(0, 0, 1)}
	if err = s.EnsureCalendarSyncStates(ctx, later, newWindow); err != nil {
		t.Fatal(err)
	}
	var ws, we time.Time
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT window_start,window_end FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&ws, &we); err != nil || !ws.Equal(state.WindowStart) || !we.Equal(state.WindowEnd) {
		t.Fatalf("source window changed during run: %v", err)
	}
	if _, err = s.GetSourceProjectionForCopy(ctx, id, state.WindowStart, state.WindowEnd, later, time.Hour*2); err != nil {
		t.Fatalf("ordinary source read blocked: %v", err)
	}
	if err = s.ReleaseExclusiveAfterConfirmedRun(ctx, activeID, []string{target}, now); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimDueCalendarSync(ctx, "source-before-run", later, later.Add(time.Hour))
	if err == nil && claimed != nil && claimed.CalendarID == target {
		if err = s.FailCalendarSync(ctx, *claimed, "transient", later, later.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		claimed, err = s.ClaimDueCalendarSync(ctx, "source-before-run", later, later.Add(time.Hour))
	}
	if err != nil || claimed == nil || claimed.CalendarID != id {
		t.Fatalf("source claim after release: %v", err)
	}
	// If the Run acquires the key just after source claim, a conflict is
	// deferred without losing the previously verified cursor or readiness.
	activeID = racingRunID
	if err = s.BeginExclusiveDispatch(ctx, ExclusiveDispatchRequest{OperationID: activeID, TargetKey: GlobalSyncDispatchKey, RequestHash: projectionPageHash([]byte("race")), Kind: "run", CalendarIDs: []string{target}, Now: later}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeferCalendarSyncForReservation(ctx, *claimed, later, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetSourceProjectionForCopy(ctx, id, state.WindowStart, state.WindowEnd, later.Add(time.Second), 2*time.Hour); err != nil {
		t.Fatalf("defer did not restore verified source: %v", err)
	}
	if err = s.ReleaseExclusiveAfterConfirmedRun(ctx, activeID, []string{target}, later); err != nil {
		t.Fatal(err)
	}
}
