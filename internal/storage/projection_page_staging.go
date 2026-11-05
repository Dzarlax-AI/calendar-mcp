package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"calendar-mcp/internal/calendar"
)

var ErrProjectionPageStage = errors.New("projection page staging rejected")

const projectionPageLimit = 250
const projectionPageSizeLimit = 32 << 20
const projectionAttemptSizeLimit = 32 << 20
const projectionAttemptCountLimit = 8

// ProjectionPageAttemptInput describes a private copy-safe candidate, not a
// published revision. The sync cursor is provider data and must not be logged.
type ProjectionPageAttemptInput struct {
	CalendarID, AttemptID, Timezone      string
	Generation, GuardEpoch, BaseRevision int64
	Mode                                 calendar.EventSyncMode
	BaseCursor                           string
	WindowStart, WindowEnd               time.Time
}

// StartProjectionPageAttempt checks the existing lease, frozen provider state,
// guard, and a matching complete base for incremental mode in a single
// transaction. The current cached_events table is never used as a base.
// No attempt is published or usable by a copy rule. Caller supplies a trusted
// clock sample; monotonic elapsed time is added after database lock waits.
// Lease checks do not constitute authorization for any provider write.
func (s *Store) StartProjectionPageAttempt(ctx context.Context, state CalendarSyncState, in ProjectionPageAttemptInput, now time.Time) error {
	if state.CalendarID == "" || in.CalendarID != state.CalendarID || state.LeaseOwner == "" || in.Generation != state.Generation || !validProjectionMutationID(in.AttemptID) || in.GuardEpoch < 0 || in.BaseRevision < 0 || in.WindowStart.IsZero() || !in.WindowEnd.After(in.WindowStart) || now.IsZero() || len(in.BaseCursor) > 4096 {
		return ErrProjectionPageStage
	}
	if _, err := time.LoadLocation(in.Timezone); in.Timezone == "" || in.Timezone == "Local" || err != nil {
		return ErrProjectionPageStage
	}
	if in.Mode != calendar.EventSyncIncremental && in.Mode != calendar.EventSyncReplacement {
		return ErrProjectionPageStage
	}
	if (in.Mode == calendar.EventSyncReplacement && in.BaseCursor != "") || (in.Mode == calendar.EventSyncIncremental && (in.BaseCursor == "" || in.BaseRevision == 0)) {
		return ErrProjectionPageStage
	}
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin page stage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockProjectionPageSource(ctx, s, tx, state.CalendarID); err != nil {
		return err
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	var cursor, timezone, provider, connectionStatus string
	var ws, we time.Time
	var enabled, readable bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT st.cursor,st.window_start,st.window_end,c.timezone,c.enabled,c.can_read,conn.provider,conn.status FROM calendar_sync_state st JOIN calendars c ON c.id=st.calendar_id JOIN connections conn ON conn.id=c.connection_id WHERE st.calendar_id=?`), in.CalendarID).Scan(&cursor, &ws, &we, &timezone, &enabled, &readable, &provider, &connectionStatus)
	if err != nil {
		return fmt.Errorf("read page stage source: %w", err)
	}
	if !enabled || !readable || provider != "google" || connectionStatus != "connected" || cursor != in.BaseCursor || !ws.Equal(in.WindowStart) || !we.Equal(in.WindowEnd) || timezone != in.Timezone {
		return ErrProjectionPageStage
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0) ON CONFLICT(calendar_id) DO NOTHING`), in.CalendarID); err != nil {
		return fmt.Errorf("initialize page stage guard: %w", err)
	}
	if err = lockProjectionSnapshotGuard(ctx, s, tx, in.CalendarID, in.GuardEpoch, in.BaseRevision); err != nil {
		return err
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	if in.BaseRevision > 0 {
		var baseCursor, baseTZ string
		var baseStart, baseEnd time.Time
		var baseEpoch int64
		var baseVerified bool
		err = tx.QueryRowContext(ctx, s.query(`SELECT cursor,timezone,window_start,window_end,guard_epoch,verified FROM projection_snapshots WHERE calendar_id=? AND revision=?`), in.CalendarID, in.BaseRevision).Scan(&baseCursor, &baseTZ, &baseStart, &baseEnd, &baseEpoch, &baseVerified)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProjectionPageStage
		}
		if err != nil {
			return fmt.Errorf("read page stage base: %w", err)
		}
		if in.Mode == calendar.EventSyncIncremental && (!baseVerified || baseCursor != in.BaseCursor || baseTZ != in.Timezone || !baseStart.Equal(in.WindowStart) || !baseEnd.Equal(in.WindowEnd) || baseEpoch != in.GuardEpoch) {
			return ErrProjectionPageStage
		}
	}
	// Every older generation is permanently fenced by the currently held
	// lease. Its staging cannot be published and need not exhaust the bounded
	// attempt capacity after a restart or degraded provider page.
	if _, err = tx.ExecContext(ctx, s.query(`DELETE FROM projection_page_attempts WHERE calendar_id=? AND generation<?`), in.CalendarID, in.Generation); err != nil {
		return fmt.Errorf("discard superseded page attempts: %w", err)
	}
	var count int
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_page_attempts WHERE calendar_id=?`), in.CalendarID).Scan(&count); err != nil {
		return fmt.Errorf("count page attempts: %w", err)
	}
	if count >= projectionAttemptCountLimit {
		return ErrProjectionPageStage
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_page_attempts(calendar_id,generation,attempt_id,lease_owner,mode,base_revision,base_cursor,guard_epoch,window_start,window_end,timezone,next_token_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), in.CalendarID, in.Generation, in.AttemptID, state.LeaseOwner, string(in.Mode), in.BaseRevision, in.BaseCursor, in.GuardEpoch, in.WindowStart.UTC(), in.WindowEnd.UTC(), in.Timezone, projectionPageHash(nil))
	if err != nil {
		return fmt.Errorf("create page stage: %w", err)
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit page stage: %w", err)
	}
	return nil
}

// lockProjectionPageSource follows the shared lock order: calendar, connection,
// sync lease, then projection guard. The connection precedes the lease because
// verification updates lock connection before reactivating sync state.
func lockProjectionPageSource(ctx context.Context, s *Store, tx *sql.Tx, calendarID string) error {
	if s.dialect == DialectSQLite {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendars SET enabled=enabled WHERE id=?`), calendarID); err != nil {
			return fmt.Errorf("lock page stage calendar: %w", err)
		}
	}
	q := `SELECT connection_id FROM calendars WHERE id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var connectionID string
	err := tx.QueryRowContext(ctx, s.query(q), calendarID).Scan(&connectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProjectionPageStage
	}
	if err != nil {
		return fmt.Errorf("lock page stage calendar: %w", err)
	}
	q = `SELECT id FROM connections WHERE id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR UPDATE`
	}
	var lockedConnection string
	err = tx.QueryRowContext(ctx, s.query(q), connectionID).Scan(&lockedConnection)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProjectionPageStage
	}
	if err != nil {
		return fmt.Errorf("lock page stage connection: %w", err)
	}
	return nil
}

func projectionPageHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// AppendProjectionPage stores one canonical Google expanded-events page under
// its live lease. Page tokens are stored only as hashes, cursor only on the
// final attempt. It verifies page-chain replay, not provider completeness.
func (s *Store) AppendProjectionPage(ctx context.Context, state CalendarSyncState, attemptID string, ordinal int, previousToken, observedTimezone string, page calendar.EventSyncPage, now time.Time) error {
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append page stage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockProjectionPageSource(ctx, s, tx, state.CalendarID); err != nil {
		return err
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	err = s.appendProjectionPageTx(ctx, tx, state, attemptID, ordinal, previousToken, observedTimezone, page, now, started)
	if errors.Is(err, ErrProjectionPageAlreadyStaged) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit page stage: %w", err)
	}
	return nil
}

// ErrProjectionPageAlreadyStaged means the page exists. It does not prove
// whether the legacy projection was updated, so combined callers stop.
var ErrProjectionPageAlreadyStaged = errors.New("projection page already staged; legacy application unconfirmed")

// appendProjectionPageTx needs the source and sync lease locks, and acquires the
// guard in the established order. It never commits the surrounding transaction.
func (s *Store) appendProjectionPageTx(ctx context.Context, tx *sql.Tx, state CalendarSyncState, attemptID string, ordinal int, previousToken, observedTimezone string, page calendar.EventSyncPage, now, started time.Time) error {
	if state.CalendarID == "" || state.LeaseOwner == "" || !validProjectionMutationID(attemptID) || ordinal < 0 || ordinal >= projectionPageLimit || len(previousToken) > 4096 || len(page.NextPageToken) > 4096 || len(page.NextCursor) > 4096 || now.IsZero() {
		return ErrProjectionPageStage
	}
	if page.SourceTimeZone == "" || page.SourceTimeZone != observedTimezone {
		return ErrProjectionPageStage
	}
	if err := validateProjectionGooglePage(state.CalendarID, page); err != nil {
		return err
	}
	prevHash, nextHash := projectionPageHash([]byte(previousToken)), projectionPageHash([]byte(page.NextPageToken))
	terminalCursor := string(page.NextCursor)
	// Persist no page token or cursor in the page payload.
	page.NextPageToken = ""
	page.NextCursor = ""
	payload, err := encodeProjectionPagePayload(page, state.CalendarID)
	if err != nil || len(payload) > projectionPageSizeLimit {
		return ErrProjectionPageStage
	}
	digestInput, err := json.Marshal(struct {
		Payload                json.RawMessage
		Previous, Next, Cursor string
	}{payload, prevHash, nextHash, terminalCursor})
	if err != nil {
		return ErrProjectionPageStage
	}
	digest := projectionPageHash(digestInput)
	var owner, attemptCursor, attemptTZ string
	var attemptStart, attemptEnd time.Time
	var epoch, base int64
	var nextOrdinal int
	var expectedToken string
	var total int64
	var complete bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT lease_owner,guard_epoch,base_revision,next_ordinal,next_token_hash,total_bytes,complete,base_cursor,timezone,window_start,window_end FROM projection_page_attempts WHERE calendar_id=? AND generation=? AND attempt_id=?`), state.CalendarID, state.Generation, attemptID).Scan(&owner, &epoch, &base, &nextOrdinal, &expectedToken, &total, &complete, &attemptCursor, &attemptTZ, &attemptStart, &attemptEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProjectionPageStage
	}
	if err != nil {
		return fmt.Errorf("read page attempt: %w", err)
	}
	if owner != state.LeaseOwner || observedTimezone == "" || observedTimezone != attemptTZ {
		return ErrProjectionPageStage
	}
	var sourceCursor, sourceTZ, sourceProvider, connectionStatus string
	var sourceStart, sourceEnd time.Time
	var enabled, readable bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT st.cursor,st.window_start,st.window_end,c.timezone,c.enabled,c.can_read,conn.provider,conn.status FROM calendar_sync_state st JOIN calendars c ON c.id=st.calendar_id JOIN connections conn ON conn.id=c.connection_id WHERE st.calendar_id=?`), state.CalendarID).Scan(&sourceCursor, &sourceStart, &sourceEnd, &sourceTZ, &enabled, &readable, &sourceProvider, &connectionStatus)
	if err != nil {
		return fmt.Errorf("read page stage source: %w", err)
	}
	if !enabled || !readable || sourceProvider != "google" || connectionStatus != "connected" || sourceCursor != attemptCursor || sourceTZ != attemptTZ || !sourceStart.Equal(attemptStart) || !sourceEnd.Equal(attemptEnd) {
		return ErrProjectionPageStage
	}
	if err = lockProjectionSnapshotGuard(ctx, s, tx, state.CalendarID, epoch, base); err != nil {
		return err
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	if ordinal < nextOrdinal {
		var oldHash string
		err = tx.QueryRowContext(ctx, s.query(`SELECT page_hash FROM projection_page_attempt_pages WHERE calendar_id=? AND generation=? AND page_ordinal=?`), state.CalendarID, state.Generation, ordinal).Scan(&oldHash)
		if err != nil || oldHash != digest {
			return ErrProjectionPageStage
		}
		return ErrProjectionPageAlreadyStaged
	}
	if complete || ordinal != nextOrdinal || expectedToken != prevHash || total+int64(len(payload)) > projectionAttemptSizeLimit {
		return ErrProjectionPageStage
	}
	if !page.Complete {
		var repeated bool
		if nextHash == prevHash {
			return ErrProjectionPageStage
		}
		if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_page_attempt_pages WHERE calendar_id=? AND generation=? AND next_token_hash=?)`), state.CalendarID, state.Generation, nextHash).Scan(&repeated); err != nil {
			return fmt.Errorf("check page token chain: %w", err)
		}
		if repeated {
			return ErrProjectionPageStage
		}
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_page_attempt_pages(calendar_id,generation,page_ordinal,page_hash,previous_token_hash,next_token_hash,payload) VALUES(?,?,?,?,?,?,?)`), state.CalendarID, state.Generation, ordinal, digest, prevHash, nextHash, payload); err != nil {
		return fmt.Errorf("insert staged page: %w", err)
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE projection_page_attempts SET next_ordinal=?,next_token_hash=?,total_bytes=?,complete=?,final_cursor=? WHERE calendar_id=? AND generation=? AND attempt_id=?`), ordinal+1, nextHash, total+int64(len(payload)), page.Complete, terminalCursor, state.CalendarID, state.Generation, attemptID); err != nil {
		return fmt.Errorf("advance page stage: %w", err)
	}
	if err = lockCalendarSyncLease(ctx, s, tx, state, now.Add(time.Since(started))); err != nil {
		return err
	}
	return nil
}

func validateProjectionGooglePage(calendarID string, page calendar.EventSyncPage) error {
	if page.ResetRequired || len(page.Warnings) > 0 || len(page.Upserts) > 2500 || len(page.DeletedEventIDs) > 2500 || len(page.DeletedObjectIDs) > 0 || len(page.ReplacedObjectIDs) > 0 || len(page.Inventory) > 0 {
		return ErrProjectionPageStage
	}
	if page.Complete {
		if page.NextPageToken != "" || page.NextCursor == "" {
			return ErrProjectionPageStage
		}
	} else if page.NextPageToken == "" || page.NextCursor != "" {
		return ErrProjectionPageStage
	}
	deleted := make(map[string]struct{}, len(page.DeletedEventIDs))
	for _, id := range page.DeletedEventIDs {
		if id == "" {
			return ErrProjectionPageStage
		}
		if _, exists := deleted[id]; exists {
			return ErrProjectionPageStage
		}
		deleted[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(page.Removals))
	for _, rem := range page.Removals {
		if rem.ObjectID == "" || (rem.Reason != calendar.EventSyncProviderCancellation && rem.Reason != calendar.EventSyncWindowEviction) {
			return ErrProjectionPageStage
		}
		if _, ok := deleted[rem.ObjectID]; !ok {
			return ErrProjectionPageStage
		}
		if _, ok := seen[rem.ObjectID]; ok {
			return ErrProjectionPageStage
		}
		seen[rem.ObjectID] = struct{}{}
	}
	if len(seen) != len(deleted) {
		return ErrProjectionPageStage
	}
	upserts := make(map[string]struct{}, len(page.Upserts))
	for _, item := range page.Upserts {
		if item.Event.ID == "" || item.Event.CalendarID != calendarID || item.Object.ObjectID != item.Event.ID || item.Event.Status == "cancelled" || item.Event.InstanceKind == "cancelled" || item.Event.InstanceKind == "seriesMaster" || len(item.Event.Recurrence) > 0 {
			return ErrProjectionPageStage
		}
		if _, ok := deleted[item.Event.ID]; ok {
			return ErrProjectionPageStage
		}
		if _, ok := upserts[item.Event.ID]; ok {
			return ErrProjectionPageStage
		}
		upserts[item.Event.ID] = struct{}{}
	}
	return nil
}

// ProjectionPageAttemptInspection is diagnostic storage state. It does not
// establish a live lease, completeness, publication, or permission to write.
// BaseCursor and FinalCursor are private provider values; do not log them.
type ProjectionPageAttemptInspection struct {
	Input       ProjectionPageAttemptInput
	LeaseOwner  string
	NextOrdinal int
	Complete    bool
	FinalCursor string
}

// GetProjectionPageAttemptByID supports investigation of an ambiguous Start
// commit. Absence alone does not prove an in-flight commit cannot still land.
func (s *Store) GetProjectionPageAttemptByID(ctx context.Context, calendarID, attemptID string) (ProjectionPageAttemptInspection, error) {
	var out ProjectionPageAttemptInspection
	if calendarID == "" || !validProjectionMutationID(attemptID) {
		return out, ErrProjectionPageStage
	}
	out.Input.CalendarID, out.Input.AttemptID = calendarID, attemptID
	var mode string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT generation,lease_owner,mode,base_revision,base_cursor,guard_epoch,window_start,window_end,timezone,next_ordinal,complete,final_cursor FROM projection_page_attempts WHERE calendar_id=? AND attempt_id=?`), calendarID, attemptID).Scan(&out.Input.Generation, &out.LeaseOwner, &mode, &out.Input.BaseRevision, &out.Input.BaseCursor, &out.Input.GuardEpoch, &out.Input.WindowStart, &out.Input.WindowEnd, &out.Input.Timezone, &out.NextOrdinal, &out.Complete, &out.FinalCursor)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectionPageAttemptInspection{}, ErrNotFound
	}
	if err != nil {
		return ProjectionPageAttemptInspection{}, fmt.Errorf("inspect page attempt: %w", err)
	}
	out.Input.Mode = calendar.EventSyncMode(mode)
	return out, nil
}
