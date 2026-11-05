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

// projectionUntrustedFinalizationCandidate is private transaction-scoped data. It does
// not establish provider provenance or a writer/rollout barrier and must never
// be used as authority to publish a verified snapshot.
type projectionUntrustedFinalizationCandidate struct {
	input    ProjectionPageAttemptInput
	cursor   string
	pages    []calendar.EventSyncPage
	events   []calendar.EventV2
	removals []calendarprojection.RemovalObservation
}

// prepareProjectionFinalizationTx validates and materializes one complete
// attempt while holding source, lease and projection locks. Caller must keep
// the transaction open. In particular, this deliberately does not publish,
// advance a cursor, or establish the still-missing live-writer barrier. A page
// stored through AppendProjectionPage may never have reached the legacy cache;
// therefore any future finalizer must replay every page in its transaction.
// The candidate is explicitly untrusted and provides no publication authority.
func (s *Store) prepareProjectionFinalizationTx(ctx context.Context, tx *sql.Tx, state CalendarSyncState, attemptID string, now time.Time) (projectionUntrustedFinalizationCandidate, error) {
	var out projectionUntrustedFinalizationCandidate
	if state.CalendarID == "" || state.LeaseOwner == "" || !validProjectionMutationID(attemptID) || now.IsZero() {
		return out, ErrProjectionPageStage
	}
	started := time.Now()
	if err := lockProjectionPageSource(ctx, s, tx, state.CalendarID); err != nil {
		return out, err
	}
	if err := lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return out, err
	}
	in := ProjectionPageAttemptInput{CalendarID: state.CalendarID, Generation: state.Generation, AttemptID: attemptID}
	var owner, mode, lastHash, finalCursor string
	var ordinal int
	var total int64
	var complete bool
	err := tx.QueryRowContext(ctx, s.query(`SELECT lease_owner,mode,base_revision,base_cursor,guard_epoch,window_start,window_end,timezone,next_ordinal,next_token_hash,total_bytes,complete,final_cursor FROM projection_page_attempts WHERE calendar_id=? AND generation=? AND attempt_id=?`), state.CalendarID, state.Generation, attemptID).Scan(&owner, &mode, &in.BaseRevision, &in.BaseCursor, &in.GuardEpoch, &in.WindowStart, &in.WindowEnd, &in.Timezone, &ordinal, &lastHash, &total, &complete, &finalCursor)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrProjectionPageStage
	}
	if err != nil {
		return out, fmt.Errorf("read candidate attempt: %w", err)
	}
	in.Mode = calendar.EventSyncMode(mode)
	if owner != state.LeaseOwner || !complete || ordinal < 1 || ordinal > projectionPageLimit || (in.Mode != calendar.EventSyncIncremental && in.Mode != calendar.EventSyncReplacement) {
		return out, ErrProjectionPageStage
	}
	var sourceCursor, timezone, provider, status string
	var ws, we time.Time
	var enabled, readable, quarantine bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT st.cursor,st.window_start,st.window_end,c.timezone,c.enabled,c.can_read,conn.provider,conn.status,EXISTS(SELECT 1 FROM calendar_sync_quarantine q WHERE q.calendar_id=c.id AND q.active=?) FROM calendar_sync_state st JOIN calendars c ON c.id=st.calendar_id JOIN connections conn ON conn.id=c.connection_id WHERE st.calendar_id=?`), true, state.CalendarID).Scan(&sourceCursor, &ws, &we, &timezone, &enabled, &readable, &provider, &status, &quarantine)
	if err != nil {
		return out, fmt.Errorf("read candidate source: %w", err)
	}
	if !enabled || !readable || provider != "google" || status != "connected" || quarantine || sourceCursor != in.BaseCursor || timezone != in.Timezone || !ws.Equal(in.WindowStart) || !we.Equal(in.WindowEnd) {
		return out, ErrProjectionPageStage
	}
	if err = lockProjectionSnapshotGuard(ctx, s, tx, state.CalendarID, in.GuardEpoch, in.BaseRevision); err != nil {
		return out, err
	}
	var base []calendar.EventV2
	if in.Mode == calendar.EventSyncIncremental {
		var baseCursor, baseTZ string
		var baseWS, baseWE time.Time
		var epoch int64
		var verified bool
		var payload []byte
		err = tx.QueryRowContext(ctx, s.query(`SELECT cursor,timezone,window_start,window_end,guard_epoch,verified,payload FROM projection_snapshots WHERE calendar_id=? AND revision=?`), state.CalendarID, in.BaseRevision).Scan(&baseCursor, &baseTZ, &baseWS, &baseWE, &epoch, &verified, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrProjectionPageStage
		}
		if err != nil {
			return out, fmt.Errorf("read candidate base: %w", err)
		}
		if !verified || baseCursor != in.BaseCursor || baseTZ != in.Timezone || !baseWS.Equal(in.WindowStart) || !baseWE.Equal(in.WindowEnd) || epoch != in.GuardEpoch {
			return out, ErrProjectionPageStage
		}
		base, err = decodeProjectionSnapshotPayload(payload, state.CalendarID)
		if err != nil {
			return out, err
		}
	} else if in.BaseCursor != "" {
		return out, ErrProjectionPageStage
	}
	rows, err := tx.QueryContext(ctx, s.query(`SELECT page_ordinal,page_hash,previous_token_hash,next_token_hash,payload FROM projection_page_attempt_pages WHERE calendar_id=? AND generation=? ORDER BY page_ordinal`), state.CalendarID, state.Generation)
	if err != nil {
		return out, fmt.Errorf("read candidate pages: %w", err)
	}
	var records []projectionStagedPage
	for rows.Next() {
		var p projectionStagedPage
		if err = rows.Scan(&p.ordinal, &p.digest, &p.previous, &p.next, &p.payload); err != nil {
			_ = rows.Close()
			return out, err
		}
		records = append(records, p)
		if len(records) > projectionPageLimit {
			_ = rows.Close()
			return out, ErrProjectionPageStage
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	if err = rows.Close(); err != nil {
		return out, err
	}
	if len(records) != ordinal {
		return out, ErrProjectionPageStage
	}
	pages, err := validateProjectionPageChain(state.CalendarID, in.Timezone, finalCursor, lastHash, total, records)
	if err != nil {
		return out, err
	}
	events, removals, err := calendarprojection.Materialize(state.CalendarID, in.Timezone, in.WindowStart, in.WindowEnd, in.Mode, base, in.Mode == calendar.EventSyncIncremental, pages)
	if err != nil {
		return out, fmt.Errorf("materialize candidate: %w", err)
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return out, err
	}
	return projectionUntrustedFinalizationCandidate{input: in, cursor: finalCursor, pages: pages, events: events, removals: removals}, nil
}
