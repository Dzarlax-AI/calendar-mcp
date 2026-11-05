package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
)

type SourceProjectionForCopy struct {
	Snapshot     ProjectionSnapshot
	Events       []calendar.EventV2
	Observations []calendarprojection.RemovalObservation
}

// GetSourceProjectionForCopy obtains the latest snapshot and its cumulative
// removal journal from one database statement. It can become stale as soon as
// it returns; a target writer must recheck immediately before dispatch and
// must independently fence concurrent/in-flight operations.
func (s *Store) GetSourceProjectionForCopy(ctx context.Context, id string, start, end, now time.Time, maxAge time.Duration) (SourceProjectionForCopy, error) {
	return s.getSourceProjectionForCopy(ctx, s.db, id, start, end, now, maxAge)
}

func (s *Store) getSourceProjectionForCopy(ctx context.Context, db cacheEligibilityQueryer, id string, start, end, now time.Time, maxAge time.Duration) (SourceProjectionForCopy, error) {
	var out SourceProjectionForCopy
	if id == "" || start.IsZero() || !end.After(start) || now.IsZero() || maxAge <= 0 {
		return out, ErrProjectionSnapshotUnavailable
	}
	var p ProjectionSnapshot
	p.CalendarID = id
	var raw []byte
	var epoch int64
	var pending, quarantined, enabled, readable bool
	var tz, connectionStatus, provider, status, cursor string
	var ws, we time.Time
	var success sql.NullTime
	err := db.QueryRowContext(ctx, s.query(`SELECT p.revision,p.stage_id,p.guard_epoch,p.base_revision,p.window_start,p.window_end,p.completed_at,p.timezone,p.cursor,p.payload,p.verified,v.observations,g.epoch,EXISTS(SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=g.calendar_id),EXISTS(SELECT 1 FROM calendar_sync_quarantine q WHERE q.calendar_id=g.calendar_id AND q.active=?),c.enabled,c.can_read,c.timezone,conn.status,conn.provider,st.status,st.cursor,st.window_start,st.window_end,st.last_success_at FROM projection_mutation_guards g JOIN projection_snapshots p ON p.calendar_id=g.calendar_id AND p.revision=g.revision JOIN projection_revision_provenance v ON v.calendar_id=p.calendar_id AND v.revision=p.revision JOIN calendars c ON c.id=g.calendar_id JOIN connections conn ON conn.id=c.connection_id JOIN calendar_sync_state st ON st.calendar_id=g.calendar_id WHERE g.calendar_id=?`), true, id).Scan(&p.Revision, &p.StageID, &p.GuardEpoch, &p.BaseRevision, &p.WindowStart, &p.WindowEnd, &p.CompletedAt, &p.Timezone, &p.Cursor, &p.Payload, &p.Verified, &raw, &epoch, &pending, &quarantined, &enabled, &readable, &tz, &connectionStatus, &provider, &status, &cursor, &ws, &we, &success)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrProjectionSnapshotUnavailable
	}
	if err != nil {
		return out, fmt.Errorf("read source projection: %w", err)
	}
	_, zoneErr := time.LoadLocation(tz)
	if !p.Verified || tz == "" || tz == "Local" || zoneErr != nil || tz != p.Timezone || !enabled || !readable || connectionStatus != "connected" || provider != "google" || status != "ready" || pending || quarantined || epoch != p.GuardEpoch || p.Cursor == "" || cursor != p.Cursor || !success.Valid || success.Time.Before(p.CompletedAt) || !p.WindowStart.Equal(ws) || !p.WindowEnd.Equal(we) || p.WindowStart.After(start) || p.WindowEnd.Before(end) || p.CompletedAt.After(now) || now.Sub(p.CompletedAt) > maxAge || len(raw) == 0 || len(raw) > maxProjectionObservationBytes {
		return out, ErrProjectionSnapshotUnavailable
	}
	events, err := decodeProjectionSnapshotPayload(p.Payload, id)
	if err != nil {
		return out, ErrProjectionSnapshotUnavailable
	}
	var observations []calendarprojection.RemovalObservation
	if err = decodeProjectionPayload(raw, maxProjectionObservationBytes, &observations); err != nil {
		return out, ErrProjectionSnapshotUnavailable
	}
	for _, o := range observations {
		if o.Removal.ObjectID == "" || (o.Removal.Reason != calendar.EventSyncProviderCancellation && o.Removal.Reason != calendar.EventSyncWindowEviction) || (!o.Unresolved && o.PreviousIdentity == "") {
			return out, ErrProjectionSnapshotUnavailable
		}
	}
	return SourceProjectionForCopy{Snapshot: p, Events: events, Observations: observations}, nil
}
func (s *Store) CheckSourceProjectionCurrent(ctx context.Context, id string, revision int64, start, end, now time.Time, maxAge time.Duration) error {
	value, err := s.GetSourceProjectionForCopy(ctx, id, start, end, now, maxAge)
	if err != nil {
		return err
	}
	if revision < 1 || value.Snapshot.Revision != revision {
		return ErrProjectionSnapshotUnavailable
	}
	return nil
}
