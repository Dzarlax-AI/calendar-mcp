package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrProjectionSnapshotUnavailable does not identify which private source
// condition failed. Callers must stop rather than substitute a provider read.
var ErrProjectionSnapshotUnavailable = errors.New("projection snapshot is unavailable")

// ProjectionSnapshotInspection is a point-in-time diagnostic observation.
// It is not proof of provider completeness or permission to mutate a target:
// publishing is currently disconnected from sync leases and page provenance,
// and no pin or target-write fencing is performed.
type ProjectionSnapshotInspection struct {
	Snapshot         ProjectionSnapshot
	CalendarTimezone string
}

// InspectProjectionSnapshotCandidate evaluates all listed conditions against a
// single SQL statement's database snapshot. The observation can become stale
// immediately after return and cannot authorize writes or be used as a pin.
// A stale/pending/disabled/disconnected/incomplete source fails closed.
func (s *Store) InspectProjectionSnapshotCandidate(ctx context.Context, calendarID string, windowStart, windowEnd, now time.Time, maxAge time.Duration) (ProjectionSnapshotInspection, error) {
	var out ProjectionSnapshotInspection
	if calendarID == "" || windowStart.IsZero() || !windowEnd.After(windowStart) || now.IsZero() || maxAge <= 0 {
		return out, errors.New("invalid projection snapshot inspection request")
	}
	var snapshot ProjectionSnapshot
	var epoch int64
	var pending, quarantined, enabled, readable bool
	var status, connectionStatus, syncCursor, timezone string
	var syncStart, syncEnd time.Time
	var syncSuccess sql.NullTime
	snapshot.CalendarID = calendarID
	err := s.db.QueryRowContext(ctx, s.query(`SELECT p.revision,p.stage_id,p.guard_epoch,p.base_revision,p.window_start,p.window_end,p.completed_at,p.timezone,p.cursor,p.payload,p.verified,
       g.epoch, EXISTS(SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=g.calendar_id),
       EXISTS(SELECT 1 FROM calendar_sync_quarantine q WHERE q.calendar_id=g.calendar_id AND q.active=?),
       c.enabled,c.can_read,c.timezone,conn.status,st.status,st.cursor,st.window_start,st.window_end,st.last_success_at
       FROM projection_mutation_guards g
       JOIN projection_snapshots p ON p.calendar_id=g.calendar_id AND p.revision=g.revision
       JOIN calendars c ON c.id=g.calendar_id
       JOIN connections conn ON conn.id=c.connection_id
       JOIN calendar_sync_state st ON st.calendar_id=g.calendar_id
       WHERE g.calendar_id=?`), true, calendarID).Scan(&snapshot.Revision, &snapshot.StageID, &snapshot.GuardEpoch, &snapshot.BaseRevision, &snapshot.WindowStart, &snapshot.WindowEnd, &snapshot.CompletedAt, &snapshot.Timezone, &snapshot.Cursor, &snapshot.Payload, &snapshot.Verified, &epoch, &pending, &quarantined, &enabled, &readable, &timezone, &connectionStatus, &status, &syncCursor, &syncStart, &syncEnd, &syncSuccess)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrProjectionSnapshotUnavailable
	}
	if err != nil {
		return out, fmt.Errorf("inspect projection snapshot candidate: %w", err)
	}
	// A changed timezone requires a new revision before using civil-day bounds.
	_, zoneErr := time.LoadLocation(timezone)
	if !snapshot.Verified || timezone == "" || timezone == "Local" || zoneErr != nil || timezone != snapshot.Timezone || !enabled || !readable || connectionStatus != "connected" || status != "ready" || pending || quarantined || epoch != snapshot.GuardEpoch || snapshot.Cursor == "" || syncCursor != snapshot.Cursor || !syncSuccess.Valid || syncSuccess.Time.Before(snapshot.CompletedAt) || !snapshot.WindowStart.Equal(syncStart) || !snapshot.WindowEnd.Equal(syncEnd) || snapshot.WindowStart.After(windowStart) || snapshot.WindowEnd.Before(windowEnd) || snapshot.CompletedAt.After(now) || now.Sub(snapshot.CompletedAt) > maxAge {
		return out, ErrProjectionSnapshotUnavailable
	}
	out.Snapshot = snapshot
	out.CalendarTimezone = timezone
	return out, nil
}
