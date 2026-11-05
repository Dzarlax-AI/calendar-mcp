package storage

import (
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestProjectionFinalizationPrepareSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runProjectionFinalizationPrepare(t, s, "calendar", now)
}
func runProjectionFinalizationPrepare(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "UTC", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "prepare-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	in := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "prepare-attempt-0123456789", Timezone: "UTC", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	if err = s.StartProjectionPageAttempt(ctx, *state, in, now); err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "event", CalendarID: id, Provider: "google", Start: calendar.EventTime{DateTime: now.Add(-time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "cursor", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: event.ID}, Event: event}}}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "UTC", page, now); err != nil {
		t.Fatal(err)
	}
	check := func() (projectionUntrustedFinalizationCandidate, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		return s.prepareProjectionFinalizationTx(ctx, tx, *state, in.AttemptID, now)
	}
	result, err := check()
	if err != nil || len(result.events) != 1 || result.cursor != "cursor" {
		t.Fatalf("prepare: events=%d err=%v", len(result.events), err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM cached_events WHERE calendar_id=? AND event_id=?`), id, event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("standalone staging unexpectedly applied cache: %d %v", count, err)
	}
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_snapshots WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected publication: %d %v", count, err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE projection_page_attempt_pages SET page_hash=? WHERE calendar_id=?`), "bad", id); err != nil {
		t.Fatal(err)
	}
	if _, err = check(); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("corrupt page accepted: %v", err)
	}
	var cursor, status string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&cursor, &status); err != nil || cursor != "" || status != "syncing" {
		t.Fatalf("changed source state: %v", err)
	}
}
