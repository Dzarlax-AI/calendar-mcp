package storage

import (
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestProjectionAttemptCleanupSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runProjectionAttemptCleanup(t, s, "calendar", now)
}
func runProjectionAttemptCleanup(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "UTC", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "cleanup-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	in := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "cleanup-attempt-0123456789", Timezone: "UTC", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	if err = s.StartProjectionPageAttempt(ctx, *state, in, now); err != nil {
		t.Fatal(err)
	}
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "cursor"}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "UTC", page, now); err != nil {
		t.Fatal(err)
	}
	if err = s.discardObsoleteProjectionAttempt(ctx, id, state.Generation, in.AttemptID); !errors.Is(err, errProjectionAttemptNotObsolete) {
		t.Fatalf("current attempt removed: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,lease_owner=?,lease_until=? WHERE calendar_id=?`), "other", time.Now().UTC().Add(time.Hour), id); err != nil {
		t.Fatal(err)
	}
	if err = s.discardObsoleteProjectionAttempt(ctx, id, state.Generation, in.AttemptID); !errors.Is(err, errProjectionAttemptNotObsolete) {
		t.Fatalf("active lease bypass: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET lease_owner=NULL,lease_until=NULL WHERE calendar_id=?`), id); err != nil {
		t.Fatal(err)
	}
	if err = s.discardObsoleteProjectionAttempt(ctx, id, state.Generation, "different-attempt-0123456789"); !errors.Is(err, errProjectionAttemptNotObsolete) {
		t.Fatalf("wrong id: %v", err)
	}
	if err = s.discardObsoleteProjectionAttempt(ctx, id, state.Generation, in.AttemptID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_page_attempt_pages WHERE calendar_id=?`), id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("staged pages: %d %v", count, err)
	}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "UTC", page, now); err == nil {
		t.Fatal("superseded lease accepted append")
	}
}
