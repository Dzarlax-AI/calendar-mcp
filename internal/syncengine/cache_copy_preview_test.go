package syncengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type previewSnapshotReader struct {
	snapshot storage.ProjectionPreviewSnapshot
	err      error
	calls    int
	source   string
}

func (r *previewSnapshotReader) ReadProjectionPreviewSnapshot(_ context.Context, source string, _, _, _ time.Time, _ time.Duration) (storage.ProjectionPreviewSnapshot, error) {
	r.calls++
	r.source = source
	return r.snapshot, r.err
}

func TestCacheCopyPreviewUsesSnapshotOnly(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	event := calendar.EventV2{ID: "one", CalendarID: "source", Title: "Meeting", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}, Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "person@example.invalid"}}}}
	reader := &previewSnapshotReader{snapshot: storage.ProjectionPreviewSnapshot{Revision: 4, Timezone: "UTC", Events: []calendar.EventV2{event}}}
	rule := storage.Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target"}
	got, err := previewCacheCopy(t.Context(), reader, rule, now.Add(-time.Hour), now.Add(time.Hour), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || reader.source != "source" || got.Revision != 4 || got.Result.Preview.Total != 1 || got.Result.ParticipantsIncomplete != 1 {
		t.Fatalf("unexpected preview: %+v", got)
	}
	entry := got.Result.Preview.Entries[0]
	if entry.Action != "candidate" || entry.Reason != "target_not_checked" || entry.TargetStatus != "not_checked" || entry.Title != "Meeting" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestCacheCopyPreviewFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target"}
	for _, tc := range []struct {
		name   string
		reader previewSnapshotReader
	}{
		{"unavailable", previewSnapshotReader{err: storage.ErrProjectionSnapshotUnavailable}},
		{"no revision", previewSnapshotReader{snapshot: storage.ProjectionPreviewSnapshot{Timezone: "UTC"}}},
		{"bad timezone", previewSnapshotReader{snapshot: storage.ProjectionPreviewSnapshot{Revision: 1, Timezone: "Local"}}},
		{"wrong source", previewSnapshotReader{snapshot: storage.ProjectionPreviewSnapshot{Revision: 1, Timezone: "UTC", Events: []calendar.EventV2{{ID: "x", CalendarID: "other", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := previewCacheCopy(t.Context(), &tc.reader, rule, now.Add(-time.Hour), now.Add(time.Hour), now, time.Hour)
			if err == nil || got.Result.Preview != nil {
				t.Fatalf("accepted unavailable snapshot: %+v", got)
			}
			if tc.name == "unavailable" && !errors.Is(err, storage.ErrProjectionSnapshotUnavailable) {
				t.Fatal(err)
			}
		})
	}
}
