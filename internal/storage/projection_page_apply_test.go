package storage

import (
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestProjectionPageApplySQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runProjectionPageApplyContract(t, s, "calendar", now)
}

func runProjectionPageApplyContract(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "UTC", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "apply-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	attempt := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "apply-attempt-0123456789", Timezone: "UTC", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	if err := s.StartProjectionPageAttempt(ctx, *state, attempt, now); err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "one", CalendarID: id, Provider: "google", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	first := calendar.EventSyncPage{SourceTimeZone: "UTC", NextPageToken: "page-one", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: event.ID}, Event: event}}}
	invalid := first
	invalid.Upserts = []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "bad"}, Event: calendar.EventV2{ID: "bad", CalendarID: id}}}
	if err := s.ApplyProjectionStagedPage(ctx, *state, attempt.AttemptID, 0, "", "UTC", invalid, now); err == nil {
		t.Fatal("accepted invalid legacy event")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_page_attempt_pages WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stage after rollback: %d %v", count, err)
	}
	if err := s.ApplyProjectionStagedPage(ctx, *state, attempt.AttemptID, 0, "", "UTC", first, now); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM cached_events WHERE calendar_id=? AND event_id=? AND deleted=?`), id, "one", false).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy event: %d %v", count, err)
	}
	if err := s.ApplyProjectionStagedPage(ctx, *state, attempt.AttemptID, 0, "", "UTC", first, now); !errors.Is(err, ErrProjectionPageAlreadyStaged) {
		t.Fatalf("ambiguous replay: %v", err)
	}
	stale := *state
	stale.LeaseOwner = "other"
	if err := s.ApplyProjectionStagedPage(ctx, stale, attempt.AttemptID, 1, "page-one", "UTC", first, now); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale lease: %v", err)
	}
	final := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "final"}
	if err := s.ApplyProjectionStagedPage(ctx, *state, attempt.AttemptID, 1, "page-one", "UTC", final, now); !errors.Is(err, ErrProjectionFinalizerRequired) {
		t.Fatalf("unpublished final: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_page_attempt_pages WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("final altered stage: %d %v", count, err)
	}
	var status, cursor string
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT status,cursor FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&status, &cursor); err != nil || status != "syncing" || cursor != "" {
		t.Fatalf("final altered state: %s %v", status, err)
	}
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_snapshots WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("premature publication: %d %v", count, err)
	}
}
