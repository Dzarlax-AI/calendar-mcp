package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"calendar-mcp/internal/calendar"
	"github.com/google/uuid"
)

// SourceProjectionCapturePlan selects the same provider pages already used by
// event-sync. It is not a permit for any target write.
type SourceProjectionCapturePlan struct {
	Enabled, Reset bool
	Input          ProjectionPageAttemptInput
}

func (s *Store) PlanSourceProjectionCapture(ctx context.Context, state CalendarSyncState) (SourceProjectionCapturePlan, error) {
	var out SourceProjectionCapturePlan
	var provider, timezone string
	var enabled bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT conn.provider,c.timezone,EXISTS(SELECT 1 FROM sync_rules r WHERE r.source_calendar_id=c.id AND r.sync_mode=?) FROM calendars c JOIN connections conn ON conn.id=c.connection_id WHERE c.id=?`), "cached_instances", state.CalendarID).Scan(&provider, &timezone, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if !enabled || provider != "google" {
		return out, nil
	}
	out.Enabled = true
	out.Input = ProjectionPageAttemptInput{CalendarID: state.CalendarID, AttemptID: uuid.NewString(), Timezone: timezone, Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, BaseCursor: state.Cursor, Mode: calendar.EventSyncIncremental}
	var epoch, revision int64
	err = s.db.QueryRowContext(ctx, s.query(`SELECT epoch,revision FROM projection_mutation_guards WHERE calendar_id=?`), state.CalendarID).Scan(&epoch, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.Input.GuardEpoch = epoch
	out.Input.BaseRevision = revision
	if state.Cursor == "" {
		out.Input.Mode = calendar.EventSyncReplacement
		return out, nil
	}
	if revision == 0 {
		out.Reset = true
		return out, nil
	}
	var cursor, tz string
	var start, end time.Time
	var baseEpoch int64
	var verified bool
	err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,timezone,window_start,window_end,guard_epoch,verified FROM projection_snapshots WHERE calendar_id=? AND revision=?`), state.CalendarID, revision).Scan(&cursor, &tz, &start, &end, &baseEpoch, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reset = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if !verified || cursor != state.Cursor || tz != timezone || !start.Equal(state.WindowStart) || !end.Equal(state.WindowEnd) || baseEpoch != epoch {
		out.Reset = true
	}
	return out, nil
}

// FinalizeSourceProjectionPage stages and publishes a terminal page atomically.
// It is used only by the background Google sync; it does not authorize target
// writes or establish protection against old target-writing processes.
func (s *Store) FinalizeSourceProjectionPage(ctx context.Context, state CalendarSyncState, attemptID string, ordinal int, previousToken, timezone string, page calendar.EventSyncPage, now, nextSync time.Time) (int64, error) {
	return s.finalizeSourceProjectionPage(ctx, state, attemptID, ordinal, previousToken, timezone, page, now, nextSync)
}
