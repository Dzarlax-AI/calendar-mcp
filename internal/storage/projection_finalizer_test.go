package storage

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
)

func TestProjectionFinalizerSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runProjectionFinalizer(t, s, "calendar", now)
}
func runProjectionFinalizer(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	// Older synthetic fixtures may not install all experimental migrations.
	var exists bool
	if s.dialect == DialectPostgres {
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='projection_revision_provenance')`).Scan(&exists); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='projection_revision_provenance')`).Scan(&exists); err != nil {
			t.Fatal(err)
		}
	}
	if !exists {
		script, err := migrations.ReadFile("migrations/" + string(s.dialect) + "/016_projection_revision_provenance.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "UTC", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "finalizer-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	in := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "finalizer-attempt-0123456789", Timezone: "UTC", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	if err = s.StartProjectionPageAttempt(ctx, *state, in, now); err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "event", CalendarID: id, Provider: "google", Start: calendar.EventTime{DateTime: now.Add(-time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	first := calendar.EventSyncPage{SourceTimeZone: "UTC", NextPageToken: "synthetic-page", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: event.ID}, Event: event}}}
	final := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "synthetic-cursor", DeletedEventIDs: []string{"removed"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "removed", Reason: calendar.EventSyncProviderCancellation}}}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "UTC", first, now); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, "synthetic-page", "UTC", final, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.finalizeProjection(ctx, *state, in.AttemptID, now, now.Add(time.Hour)); !errors.Is(err, errProjectionWriterBarrierUnavailable) {
		t.Fatalf("barrier: %v", err)
	}
	// The lease expires after the final sweep/CAS started. The final post-write
	// clock sample must reject the result, and rollback must restore every row.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = s.finalizeProjectionTxWithClock(ctx, tx, *state, in.AttemptID, now, now.Add(2*time.Hour), func() time.Time {
		calls++
		if calls == 1 {
			return now
		}
		return now.Add(time.Hour)
	})
	if !errors.Is(err, ErrCalendarSyncLeaseLost) {
		_ = tx.Rollback()
		t.Fatalf("expired lease: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var rollbackStatus, rollbackCursor string
	var rollbackRevision int64
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT status,cursor FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&rollbackStatus, &rollbackCursor); err != nil || rollbackStatus != "syncing" || rollbackCursor != "" {
		t.Fatalf("expired rollback sync state: %v", err)
	}
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT revision FROM projection_mutation_guards WHERE calendar_id=?`), id).Scan(&rollbackRevision); err != nil || rollbackRevision != 0 {
		t.Fatalf("expired rollback revision: %v", err)
	}
	var rollbackPages, rollbackSnapshots, rollbackCache, rollbackProvenance int
	for _, check := range []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM projection_page_attempt_pages WHERE calendar_id=?`, &rollbackPages},
		{`SELECT COUNT(*) FROM projection_snapshots WHERE calendar_id=?`, &rollbackSnapshots},
		{`SELECT COUNT(*) FROM cached_events WHERE calendar_id=?`, &rollbackCache},
		{`SELECT COUNT(*) FROM projection_revision_provenance WHERE calendar_id=?`, &rollbackProvenance},
	} {
		if err = s.db.QueryRowContext(ctx, s.query(check.query), id).Scan(check.dest); err != nil {
			t.Fatal(err)
		}
	}
	if rollbackPages != 2 || rollbackSnapshots != 0 || rollbackCache != 0 || rollbackProvenance != 0 {
		t.Fatalf("expired rollback: pages=%d snapshots=%d cache=%d provenance=%d", rollbackPages, rollbackSnapshots, rollbackCache, rollbackProvenance)
	}
	run := func(commit bool) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		revision, err := s.finalizeProjectionTx(ctx, tx, *state, in.AttemptID, now, now.Add(time.Hour))
		if err != nil || revision != 1 {
			t.Fatalf("finalize: %d %v", revision, err)
		}
		if commit {
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	run(false)
	var count int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_snapshots WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback snapshot: %d %v", count, err)
	}
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM cached_events WHERE calendar_id=? AND event_id=?`), id, event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback cache: %d %v", count, err)
	}
	run(true)
	var cursor, status string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&cursor, &status); err != nil || cursor != "synthetic-cursor" || status != "ready" {
		t.Fatalf("cursor: %v", err)
	}
	snapshot, err := s.GetProjectionSnapshot(ctx, id, 1)
	if err != nil || !snapshot.Verified {
		t.Fatalf("snapshot: %v", err)
	}
	events, err := decodeProjectionSnapshotPayload(snapshot.Payload, id)
	if err != nil || len(events) != 1 || events[0].ID != event.ID {
		t.Fatalf("events: %v", err)
	}
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM cached_events WHERE calendar_id=? AND event_id=? AND deleted=?`), id, event.ID, false).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay: %d %v", count, err)
	}
	var raw []byte
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT observations FROM projection_revision_provenance WHERE calendar_id=? AND revision=?`), id, 1).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var observations []calendarprojection.RemovalObservation
	if err = json.Unmarshal(raw, &observations); err != nil || len(observations) != 1 || observations[0].Removal.Reason != calendar.EventSyncProviderCancellation || !observations[0].Unresolved {
		t.Fatalf("provenance: %v", err)
	}
}
