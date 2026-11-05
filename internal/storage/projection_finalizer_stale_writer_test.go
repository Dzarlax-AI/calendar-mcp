package storage

import (
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestProjectionFinalizerStaleLegacyPageSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runProjectionFinalizerStaleLegacyPage(t, s, "calendar", now)
}

// This synthetic test demonstrates the missing rollout barrier. It deliberately
// invokes the internal test mechanism; the production gate remains closed.
func runProjectionFinalizerStaleLegacyPage(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "UTC", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "legacy-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	in := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "stale-writer-attempt-0123456789", Timezone: "UTC", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	if err = s.StartProjectionPageAttempt(ctx, *state, in, now); err != nil {
		t.Fatal(err)
	}
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "cursor"}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "UTC", page, now); err != nil {
		t.Fatal(err)
	}
	legacy := calendar.EventV2{ID: "stale", CalendarID: id, Provider: "google", Start: calendar.EventTime{DateTime: now.Add(-time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	if err = s.ApplyEventSyncPage(ctx, *state, EventSyncBatch{FullSync: true, Upserts: []CachedEventUpsert{{SourceObjectID: legacy.ID, Event: legacy}}}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.finalizeProjection(ctx, *state, in.AttemptID, now, now.Add(time.Hour)); !errors.Is(err, errProjectionWriterBarrierUnavailable) {
		t.Fatalf("gate: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.finalizeProjectionTx(ctx, tx, *state, in.AttemptID, now, now.Add(time.Hour))
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM cached_events WHERE calendar_id=? AND event_id=? AND deleted=?`), id, legacy.ID, false).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected synthetic stale row: %d %v", count, err)
	}
	snap, err := s.GetProjectionSnapshot(ctx, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	events, err := decodeProjectionSnapshotPayload(snap.Payload, id)
	if err != nil || len(events) != 0 {
		t.Fatalf("snapshot should differ from legacy cache: %v", err)
	}
}
