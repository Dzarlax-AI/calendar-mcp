package storage

import (
	"context"
	"time"

	"calendar-mcp/internal/calendar"
)

// ProjectionPreviewSnapshot is a point-in-time diagnostic view, not a pin or
// authorization to mutate any calendar. Its contents may become stale.
type ProjectionPreviewSnapshot struct {
	Revision int64
	Timezone string
	Events   []calendar.EventV2
}

// ReadProjectionPreviewSnapshot uses only the local store. It fails closed if
// the source is unavailable or the private snapshot envelope is malformed.
// No provider read or fallback is performed.
func (s *Store) ReadProjectionPreviewSnapshot(ctx context.Context, calendarID string, start, end, now time.Time, maxAge time.Duration) (ProjectionPreviewSnapshot, error) {
	var out ProjectionPreviewSnapshot
	candidate, err := s.InspectProjectionSnapshotCandidate(ctx, calendarID, start, end, now, maxAge)
	if err != nil {
		return out, err
	}
	events, err := decodeProjectionSnapshotPayload(candidate.Snapshot.Payload, calendarID)
	if err != nil {
		return out, ErrProjectionSnapshotUnavailable
	}
	return ProjectionPreviewSnapshot{Revision: candidate.Snapshot.Revision, Timezone: candidate.CalendarTimezone, Events: events}, nil
}
