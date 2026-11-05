package storage

import (
	"errors"
	"testing"
	"time"
)

func TestProjectionSnapshotInspectionSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	runProjectionSnapshotInspectionContract(t, s, "calendar", now)
}

// Shared by SQLite and PostgreSQL; no provider calls or real data.
func runProjectionSnapshotInspectionContract(t *testing.T, s *Store, calendarID string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	base := int64(0)
	input := ProjectionSnapshotInput{Timezone: "Europe/Belgrade", CalendarID: calendarID, StageID: "inspect-0123456789abcdef", BaseRevision: &base, WindowStart: start, WindowEnd: end, CompletedAt: now, Cursor: "synthetic-cursor", Payload: []byte(`[]`), Complete: true}
	if err := s.StageProjectionSnapshot(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, calendarID, input.StageID); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, s.query(query), args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE calendars SET enabled=?,can_read=?,timezone=? WHERE id=?`, true, true, "Europe/Belgrade", calendarID)
	exec(`UPDATE calendar_sync_state SET status=?,cursor=?,window_start=?,window_end=?,last_success_at=? WHERE calendar_id=?`, "ready", input.Cursor, start, end, now, calendarID)
	inspect := func(at time.Time, age time.Duration, from, to time.Time) error {
		_, err := s.InspectProjectionSnapshotCandidate(ctx, calendarID, from, to, at, age)
		return err
	}
	assertUnavailable := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrProjectionSnapshotUnavailable) {
			t.Fatalf("expected unavailable, got %v", err)
		}
	}
	raw, err := s.GetProjectionSnapshot(ctx, calendarID, 1)
	if err != nil || raw.Verified {
		t.Fatalf("published trust default: %v", err)
	}
	assertUnavailable(inspect(now, time.Minute, start, end))
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE projection_snapshots SET verified=? WHERE calendar_id=? AND revision=?`), true, calendarID, 1); err == nil {
		t.Fatal("trust update succeeded")
	}
	for _, query := range []string{
		`UPDATE projection_snapshots SET payload='[]' WHERE calendar_id=? AND revision=?`,
		`UPDATE projection_snapshots SET window_start=window_end WHERE calendar_id=? AND revision=?`,
		`UPDATE projection_snapshots SET cursor='changed' WHERE calendar_id=? AND revision=?`,
	} {
		if _, err := s.db.ExecContext(ctx, s.query(query), calendarID, 1); err == nil {
			t.Fatal("revision update succeeded")
		}
	}
	seedVerifiedProjectionSnapshot(t, s, calendarID, 1)
	for _, query := range []string{
		`UPDATE projection_snapshots SET payload='[]' WHERE calendar_id=? AND revision=?`,
		`UPDATE projection_snapshots SET window_start=window_end WHERE calendar_id=? AND revision=?`,
		`UPDATE projection_snapshots SET cursor='changed' WHERE calendar_id=? AND revision=?`,
		`UPDATE projection_snapshots SET verified=FALSE WHERE calendar_id=? AND revision=?`,
	} {
		if _, err := s.db.ExecContext(ctx, s.query(query), calendarID, 1); err == nil {
			t.Fatal("verified revision update succeeded")
		}
	}
	got, err := s.InspectProjectionSnapshotCandidate(ctx, calendarID, start, end, now, time.Minute)
	if err != nil || got.Snapshot.Revision != 1 || got.CalendarTimezone != "Europe/Belgrade" {
		t.Fatalf("initial inspection failed: %v", err)
	}
	assertUnavailable(inspect(now.Add(2*time.Minute), time.Minute, start, end))
	assertUnavailable(inspect(now.Add(-time.Second), time.Minute, start, end))
	assertUnavailable(inspect(now, time.Minute, start.Add(-time.Second), end))
	assertUnavailable(inspect(now, time.Minute, start, end.Add(time.Second)))
	exec(`UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`, "other", calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE calendar_sync_state SET cursor=?,status=? WHERE calendar_id=?`, input.Cursor, "syncing", calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE calendar_sync_state SET status=?,window_end=? WHERE calendar_id=?`, "ready", end.Add(time.Hour), calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE calendar_sync_state SET window_end=? WHERE calendar_id=?`, end, calendarID)
	exec(`UPDATE calendars SET timezone=? WHERE id=?`, "", calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE calendars SET timezone=? WHERE id=?`, "UTC", calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE calendars SET timezone=?,enabled=? WHERE id=?`, "Europe/Belgrade", false, calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE calendars SET enabled=?,can_read=? WHERE id=?`, true, false, calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE calendars SET can_read=? WHERE id=?`, true, calendarID)
	exec(`UPDATE connections SET status=? WHERE id=(SELECT connection_id FROM calendars WHERE id=?)`, "pending", calendarID)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`UPDATE connections SET status=? WHERE id=(SELECT connection_id FROM calendars WHERE id=?)`, "connected", calendarID)
	exec(`INSERT INTO calendar_sync_quarantine(calendar_id,object_id,error_code,first_seen_at,last_seen_at,next_repair_at,active) VALUES(?,?,?,?,?,?,?)`, calendarID, "synthetic", "protocol", now, now, now, true)
	assertUnavailable(inspect(now, time.Minute, start, end))
	exec(`DELETE FROM calendar_sync_quarantine WHERE calendar_id=?`, calendarID)
	if err := s.beginProjectionMutation(ctx, calendarID, "inspect-mutation-012345", now); err != nil {
		t.Fatal(err)
	}
	assertUnavailable(inspect(now, time.Minute, start, end))
	if err := s.resolveProjectionMutation(ctx, calendarID, "inspect-mutation-012345"); err != nil {
		t.Fatal(err)
	}
	// Clearing pending is not sufficient: the old revision belongs to an
	// earlier guard epoch and remains unusable until a clean publication.
	assertUnavailable(inspect(now, time.Minute, start, end))
}
