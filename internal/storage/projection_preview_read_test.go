package storage

import (
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestReadProjectionPreviewSnapshotSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	base := int64(0)
	events := []calendar.EventV2{{ID: "event", CalendarID: "calendar", Provider: "google", SourceAttendeesComplete: true, Start: calendar.EventTime{DateTime: now.Add(-time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}}
	payload, err := encodeProjectionSnapshotPayload(events, "calendar")
	if err != nil {
		t.Fatal(err)
	}
	in := ProjectionSnapshotInput{CalendarID: "calendar", StageID: "preview-stage-0123456789", Timezone: "UTC", BaseRevision: &base, WindowStart: start, WindowEnd: end, CompletedAt: now, Cursor: "cursor", Payload: payload, Complete: true}
	if err = s.StageProjectionSnapshot(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishProjectionSnapshot(ctx, "calendar", in.StageID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE calendars SET timezone='UTC',enabled=1,can_read=1 WHERE id='calendar'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE calendar_sync_state SET status='ready',cursor=?,window_start=?,window_end=?,last_success_at=? WHERE calendar_id='calendar'`, in.Cursor, start, end, now); err != nil {
		t.Fatal(err)
	}
	read := func(at time.Time) (ProjectionPreviewSnapshot, error) {
		return s.ReadProjectionPreviewSnapshot(ctx, "calendar", start, end, at, time.Minute)
	}
	if _, err = read(now); !errors.Is(err, ErrProjectionSnapshotUnavailable) {
		t.Fatalf("unverified accepted: %v", err)
	}
	seedVerifiedProjectionSnapshot(t, s, "calendar", 1)
	got, err := read(now)
	if err != nil || got.Revision != 1 || got.Timezone != "UTC" || len(got.Events) != 1 || !got.Events[0].SourceAttendeesComplete {
		t.Fatalf("read preview: %+v %v", got, err)
	}
	if _, err = read(now.Add(2 * time.Minute)); !errors.Is(err, ErrProjectionSnapshotUnavailable) {
		t.Fatalf("stale accepted: %v", err)
	}
}
