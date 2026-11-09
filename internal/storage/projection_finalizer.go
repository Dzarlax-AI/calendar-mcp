package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
)

var errProjectionWriterBarrierUnavailable = errors.New("projection writer barrier is unavailable")

// finalizeProjection is deliberately disabled: no runtime path may publish a
// verified snapshot until all provider and cache writers, including old
// processes, are fenced by an independently established rollout barrier.
// There is intentionally no caller-supplied boolean, token, or override.
func (s *Store) finalizeProjection(context.Context, CalendarSyncState, string, time.Time, time.Time) (int64, error) {
	return 0, errProjectionWriterBarrierUnavailable
}

// finalizeProjectionTx contains the isolated transactional mechanism only. Its
// direct use is restricted to synthetic database tests until finalizeProjection
// is equipped with an actual non-bypassable writer/rollout barrier. The caller
// owns commit/rollback. Never expose this method or call it from runtime.
func (s *Store) finalizeProjectionTx(ctx context.Context, tx *sql.Tx, state CalendarSyncState, attemptID string, now, nextSync time.Time) (int64, error) {
	started := time.Now()
	return s.finalizeProjectionTxWithClock(ctx, tx, state, attemptID, now, nextSync, func() time.Time { return now.Add(time.Since(started)) })
}

// The clock seam is private and only used by synthetic tests; the disabled
// entry point provides no way for runtime callers to supply or bypass a clock.
func (s *Store) finalizeProjectionTxWithClock(ctx context.Context, tx *sql.Tx, state CalendarSyncState, attemptID string, now, nextSync time.Time, clock func() time.Time) (int64, error) {
	if nextSync.IsZero() || !nextSync.After(now) {
		return 0, ErrProjectionPageStage
	}
	started := time.Now()
	candidate, err := s.prepareProjectionFinalizationTx(ctx, tx, state, attemptID, now)
	if err != nil {
		return 0, err
	}
	var leaseUntil time.Time
	if err = tx.QueryRowContext(ctx, s.query(`SELECT lease_until FROM calendar_sync_state WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=?`), state.CalendarID, state.Generation, "syncing", state.LeaseOwner).Scan(&leaseUntil); err != nil {
		return 0, ErrCalendarSyncLeaseLost
	}
	payload, err := encodeProjectionSnapshotPayload(candidate.events, state.CalendarID)
	if err != nil || len(payload) > maxProjectionSnapshotBytes {
		return 0, ErrProjectionPageStage
	}
	// Retain every distinct observation. Exact repetitions add no new evidence;
	// an upsert alone must never erase a cancellation while a delete is in flight.
	allRemovals := candidate.removals
	if candidate.input.BaseRevision > 0 {
		var previous []byte
		var verified bool
		err = tx.QueryRowContext(ctx, s.query(`SELECT p.observations,s.verified FROM projection_revision_provenance p JOIN projection_snapshots s ON s.calendar_id=p.calendar_id AND s.revision=p.revision WHERE p.calendar_id=? AND p.revision=?`), state.CalendarID, candidate.input.BaseRevision).Scan(&previous, &verified)
		if err != nil || !verified || len(previous) == 0 || len(previous) > maxProjectionObservationBytes {
			return 0, ErrProjectionPageStage
		}
		var retained []calendarprojection.RemovalObservation
		if err = json.Unmarshal(previous, &retained); err != nil {
			return 0, ErrProjectionPageStage
		}
		for _, item := range retained {
			if item.Removal.ObjectID == "" || (item.Removal.Reason != calendar.EventSyncProviderCancellation && item.Removal.Reason != calendar.EventSyncWindowEviction) {
				return 0, ErrProjectionPageStage
			}
		}
		allRemovals = append(retained, candidate.removals...)
	}
	allRemovals, err = uniqueProjectionObservations(allRemovals)
	if err != nil {
		return 0, ErrProjectionPageStage
	}
	observations, err := json.Marshal(allRemovals)
	if err != nil || len(observations) > maxProjectionObservationBytes {
		return 0, ErrProjectionPageStage
	}
	if err = s.releaseRetainedSourceCapacityTx(ctx, tx, state.CalendarID, candidate.input.BaseRevision, allRemovals); err != nil {
		return 0, fmt.Errorf("release old source capacity: %w", err)
	}
	var count int64
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_snapshots WHERE calendar_id=?`), state.CalendarID).Scan(&count); err != nil {
		return 0, err
	}
	if count >= maxProjectionSnapshotRevisions {
		return 0, ErrProjectionSnapshotLimit
	}
	if candidate.input.BaseRevision == math.MaxInt64 {
		return 0, ErrProjectionSnapshotLimit
	}
	revision := candidate.input.BaseRevision + 1
	completed := now.Add(time.Since(started))
	// Replay all pages without releasing the lease, including pages staged
	// without cache application. Final sweep and cursor update follow replay.
	for _, page := range candidate.pages {
		batch := EventSyncBatch{FullSync: candidate.input.Mode == calendar.EventSyncReplacement, DeletedEventIDs: append([]string(nil), page.DeletedEventIDs...), NextCursor: candidate.cursor, NextSyncAt: &nextSync}
		for _, upsert := range page.Upserts {
			batch.Upserts = append(batch.Upserts, CachedEventUpsert{SourceObjectID: upsert.Object.ObjectID, Event: upsert.Event})
			batch.Objects = append(batch.Objects, SyncObject{ObjectID: upsert.Object.ObjectID, ETag: upsert.Object.ETag})
		}
		if err = s.applyEventSyncPageTx(ctx, tx, state, batch, false, now.Add(time.Since(started))); err != nil {
			return 0, err
		}
	}
	// Immutable snapshot and provenance are inserted before the final lease
	// update; every write remains in this single uncommitted transaction.
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_snapshots(calendar_id,revision,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload,verified) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), state.CalendarID, revision, attemptID, candidate.input.GuardEpoch, candidate.input.BaseRevision, candidate.input.WindowStart, candidate.input.WindowEnd, completed, candidate.input.Timezone, candidate.cursor, payload, true)
	if err != nil {
		return 0, fmt.Errorf("insert candidate revision: %w", err)
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_revision_provenance(calendar_id,revision,observations) VALUES(?,?,?)`), state.CalendarID, revision, observations)
	if err != nil {
		return 0, fmt.Errorf("insert candidate provenance: %w", err)
	}
	res, e := tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_guards SET revision=? WHERE calendar_id=? AND epoch=? AND revision=?`), revision, state.CalendarID, candidate.input.GuardEpoch, candidate.input.BaseRevision)
	if e != nil {
		return 0, e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return 0, e
	}
	if n != 1 {
		return 0, ErrProjectionSnapshotStale
	}
	// One attempt must not be reusable after commit. Rollback restores it.
	res, err = tx.ExecContext(ctx, s.query(`DELETE FROM projection_page_attempts WHERE calendar_id=? AND generation=? AND attempt_id=?`), state.CalendarID, state.Generation, attemptID)
	if err != nil {
		return 0, err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrProjectionPageStage
	}
	if candidate.input.BaseRevision > 0 {
		// The new full snapshot carries forward all prior observations. Retire
		// the previous full payload only if no persisted reader references it.
		// Target dispatch must independently check the currently published
		// revision immediately before a write; this does not revoke in-flight IO.
		err = s.retireSourceSnapshotTx(ctx, tx, state.CalendarID, candidate.input.BaseRevision)
		if err != nil {
			return 0, fmt.Errorf("retire previous source snapshot: %w", err)
		}
	}
	// Finalize only after replay and publication writes. The final sweep may
	// itself block or take time, so check the original lease again afterwards.
	final := EventSyncBatch{FullSync: candidate.input.Mode == calendar.EventSyncReplacement, NextCursor: candidate.cursor, NextSyncAt: &nextSync}
	if err = s.applyEventSyncPageTx(ctx, tx, state, final, true, clock()); err != nil {
		return 0, err
	}
	checkedAt := clock()
	if !checkedAt.Before(leaseUntil) {
		return 0, ErrCalendarSyncLeaseLost
	}
	if !nextSync.After(checkedAt) {
		return 0, ErrProjectionPageStage
	}
	return revision, nil
}
