package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

var ErrCalendarSyncLeaseLost = errors.New("calendar sync lease is no longer owned by this worker")

// EnsureCalendarSyncStates creates projection state for every readable
// calendar. Existing cursors and leases are intentionally preserved.
func (s *Store) EnsureCalendarSyncStates(ctx context.Context, now time.Time, window SyncWindow) error {
	if !window.End.After(window.Start) {
		return errors.New("calendar sync window end must be after start")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ensure calendar sync states: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, s.query(`SELECT c.id, conn.provider
		FROM calendars c JOIN connections conn ON conn.id=c.connection_id
		WHERE c.can_read=? AND conn.status=? ORDER BY c.id`), true, "connected")
	if err != nil {
		return fmt.Errorf("list readable calendars: %w", err)
	}
	type calendarSource struct{ id, provider string }
	var calendars []calendarSource
	for rows.Next() {
		var value calendarSource
		if err := rows.Scan(&value.id, &value.provider); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan readable calendar: %w", err)
		}
		calendars = append(calendars, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate readable calendars: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range calendars {
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO calendar_sync_state(calendar_id, strategy, cursor, window_start, window_end, generation, status, next_sync_at, updated_at)
			VALUES (?, ?, '', ?, ?, 0, 'pending', ?, ?) ON CONFLICT(calendar_id) DO NOTHING`), value.id, value.provider, window.Start, window.End, now, now); err != nil {
			return fmt.Errorf("create calendar sync state: %w", err)
		}
		state, err := selectCalendarSyncStateForUpdate(ctx, s, tx, value.id)
		if err != nil {
			return fmt.Errorf("read calendar sync state: %w", err)
		}
		if state.WindowStart.Equal(window.Start) && state.WindowEnd.Equal(window.End) {
			continue
		}
		res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
			SET strategy=?, cursor='', window_start=?, window_end=?, generation=generation+1, status=?, next_sync_at=?,
				last_error_code=NULL, lease_owner=NULL, lease_until=NULL, updated_at=?
			WHERE calendar_id=? AND (lease_until IS NULL OR lease_until<=?)`), value.provider, window.Start, window.End,
			"pending", now, now, value.id, now)
		if err != nil {
			return fmt.Errorf("reset calendar sync window: %w", err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			// A current lease owns this state. Its frozen window remains valid
			// until the worker succeeds, fails, or its lease expires.
			continue
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ensure calendar sync states: %w", err)
	}
	return nil
}

// ClaimDueCalendarSync atomically takes one due or stale calendar lease.
func (s *Store) ClaimDueCalendarSync(ctx context.Context, workerID string, now, leaseUntil time.Time) (*CalendarSyncState, error) {
	if workerID == "" {
		return nil, errors.New("calendar sync worker ID is required")
	}
	if !leaseUntil.After(now) {
		return nil, errors.New("calendar sync lease must end after it starts")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin calendar sync claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := `SELECT ss.calendar_id, ss.strategy, ss.cursor, ss.window_start, ss.window_end, ss.generation, ss.status, ss.next_sync_at,
		ss.last_started_at, ss.last_success_at, ss.last_error_code, ss.lease_owner, ss.lease_until
		FROM calendar_sync_state ss
		JOIN calendars c ON c.id=ss.calendar_id
		JOIN connections conn ON conn.id=c.connection_id
		WHERE ss.status IN (?, ?, ?, ?) AND conn.status=? AND c.can_read=? AND ss.next_sync_at<=? AND (ss.lease_until IS NULL OR ss.lease_until<=?)
		ORDER BY ss.next_sync_at, ss.calendar_id LIMIT 1`
	if s.dialect == DialectPostgres {
		q += " FOR UPDATE SKIP LOCKED"
	}
	state, err := scanCalendarSyncState(tx.QueryRowContext(ctx, s.query(q), "pending", "ready", "failed", "syncing", "connected", true, now, now))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select due calendar sync: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
		SET generation=generation+1, status=?, lease_owner=?, lease_until=?, last_started_at=?, updated_at=?
		WHERE calendar_id=? AND (lease_until IS NULL OR lease_until<=?)`), "syncing", workerID, leaseUntil, now, now, state.CalendarID, now)
	if err != nil {
		return nil, fmt.Errorf("claim calendar sync: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit calendar sync claim: %w", err)
	}
	state.Generation++
	state.Status, state.LeaseOwner, state.LeaseUntil, state.LastStartedAt = "syncing", workerID, &leaseUntil, &now
	return &state, nil
}

// RenewCalendarSyncLease extends a current worker claim without changing the
// cursor or state. The new deadline must be strictly later than the stored one.
func (s *Store) RenewCalendarSyncLease(ctx context.Context, state CalendarSyncState, now, leaseUntil time.Time) error {
	if !leaseUntil.After(now) {
		return ErrCalendarSyncLeaseLost
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin renew calendar sync lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarSyncLease(ctx, s, tx, state, now); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET lease_until=?, updated_at=?
		WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>? AND lease_until<?`), leaseUntil, now,
		state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now, leaseUntil)
	if err != nil {
		return fmt.Errorf("renew calendar sync lease: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrCalendarSyncLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit renew calendar sync lease: %w", err)
	}
	return nil
}

// ApplyEventSyncPage applies a page only while its claim is current. Cursor
// replacement and a full-generation sweep happen only on the final page.
func (s *Store) ApplyEventSyncPage(ctx context.Context, state CalendarSyncState, batch EventSyncBatch, final bool, now time.Time) error {
	if state.CalendarID == "" || state.LeaseOwner == "" {
		return ErrCalendarSyncLeaseLost
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin event sync page: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarSyncLease(ctx, s, tx, state, now); err != nil {
		return err
	}
	syncedAt := now
	objectMembers := make(map[string]map[string]struct{}, len(batch.Upserts))
	for _, upsert := range batch.Upserts {
		event := upsert.Event
		if event.CalendarID == "" {
			event.CalendarID = state.CalendarID
		}
		if event.CalendarID != state.CalendarID {
			return fmt.Errorf("event %q belongs to calendar %q, not claimed calendar %q", event.ID, event.CalendarID, state.CalendarID)
		}
		sourceObjectID := upsert.SourceObjectID
		if sourceObjectID == "" {
			sourceObjectID = event.ID
		}
		if sourceObjectID == "" {
			return errors.New("cached event source object ID is required")
		}
		members := objectMembers[sourceObjectID]
		if members == nil {
			members = make(map[string]struct{})
			objectMembers[sourceObjectID] = members
		}
		members[event.ID] = struct{}{}
	}
	deletedObjects := make(map[string]struct{}, len(batch.DeletedObjectIDs))
	for _, objectID := range batch.DeletedObjectIDs {
		deletedObjects[objectID] = struct{}{}
	}
	for _, sourceObjectID := range batch.ReplacedObjectIDs {
		if sourceObjectID == "" {
			return errors.New("replaced source object ID is required")
		}
		if _, deleted := deletedObjects[sourceObjectID]; deleted {
			return fmt.Errorf("source object %q cannot be replaced and deleted in one batch", sourceObjectID)
		}
		members := objectMembers[sourceObjectID]
		if len(members) == 0 {
			return fmt.Errorf("replacement source object %q has no event membership; use DeletedObjectIDs for an empty object", sourceObjectID)
		}
		if err := tombstoneMissingObjectMembers(ctx, s, tx, state.CalendarID, sourceObjectID, members, state.Generation, syncedAt); err != nil {
			return err
		}
	}
	for _, upsert := range batch.Upserts {
		event := upsert.Event
		if event.CalendarID == "" {
			event.CalendarID = state.CalendarID
		}
		sourceObjectID := upsert.SourceObjectID
		if sourceObjectID == "" {
			sourceObjectID = event.ID
		}
		if err := upsertCachedEvent(ctx, s, tx, event, sourceObjectID, state.Generation, syncedAt); err != nil {
			return err
		}
	}
	for _, eventID := range batch.DeletedEventIDs {
		if err := tombstoneCachedEvent(ctx, s, tx, calendar.EventRef{CalendarID: state.CalendarID, EventID: eventID}, state.Generation, syncedAt); err != nil {
			return err
		}
	}
	for _, object := range batch.Objects {
		if object.ObjectID == "" {
			return errors.New("sync object ID is required")
		}
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO calendar_sync_objects(calendar_id, object_id, etag, sync_generation)
			VALUES (?, ?, ?, ?) ON CONFLICT(calendar_id, object_id) DO UPDATE SET etag=excluded.etag, sync_generation=excluded.sync_generation`), state.CalendarID, object.ObjectID, object.ETag, state.Generation); err != nil {
			return fmt.Errorf("upsert calendar sync object: %w", err)
		}
	}
	for _, objectID := range batch.DeletedObjectIDs {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE cached_events SET deleted=?, sync_generation=?, synced_at=?
			WHERE calendar_id=? AND source_object_id=? AND deleted=?`), true, state.Generation, syncedAt, state.CalendarID, objectID, false); err != nil {
			return fmt.Errorf("tombstone cached events for deleted object: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM calendar_sync_objects WHERE calendar_id=? AND object_id=?`), state.CalendarID, objectID); err != nil {
			return fmt.Errorf("delete calendar sync object: %w", err)
		}
	}
	if final {
		if batch.FullSync {
			if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM cached_events WHERE calendar_id=? AND sync_generation<>?`), state.CalendarID, state.Generation); err != nil {
				return fmt.Errorf("sweep cached event generation: %w", err)
			}
			if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM calendar_sync_objects WHERE calendar_id=? AND sync_generation<>?`), state.CalendarID, state.Generation); err != nil {
				return fmt.Errorf("sweep calendar sync object generation: %w", err)
			}
		}
		nextSyncAt := state.NextSyncAt
		if batch.NextSyncAt != nil {
			nextSyncAt = *batch.NextSyncAt
		}
		res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
			SET cursor=?, status=?, next_sync_at=?, last_success_at=?, last_error_code=NULL, lease_owner=NULL, lease_until=NULL, updated_at=?
			WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`), batch.NextCursor, "ready", nextSyncAt, syncedAt, syncedAt, state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now)
		if err != nil {
			return fmt.Errorf("finish calendar sync: %w", err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrCalendarSyncLeaseLost
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit event sync page: %w", err)
	}
	return nil
}

// FailCalendarSync releases the claim without changing the authoritative
// cursor, allowing the next worker to safely replay from that cursor.
func (s *Store) FailCalendarSync(ctx context.Context, state CalendarSyncState, code string, now, next time.Time) error {
	if state.CalendarID == "" || state.LeaseOwner == "" {
		return ErrCalendarSyncLeaseLost
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed calendar sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarSyncLease(ctx, s, tx, state, now); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
		SET status=?, next_sync_at=?, last_error_code=?, lease_owner=NULL, lease_until=NULL, updated_at=?
		WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`), "failed", next, nullString(code), now, state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now)
	if err != nil {
		return fmt.Errorf("fail calendar sync: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrCalendarSyncLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed calendar sync: %w", err)
	}
	return nil
}

// ParkCalendarSync stops retrying a calendar until an explicit reconnect or
// configuration action reactivates it. The opaque cursor remains authoritative.
func (s *Store) ParkCalendarSync(ctx context.Context, state CalendarSyncState, code string, now time.Time) error {
	if state.CalendarID == "" || state.LeaseOwner == "" {
		return ErrCalendarSyncLeaseLost
	}
	if !safeCalendarSyncCode(code) {
		return ErrInvalidSyncCode
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin park calendar sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarSyncLease(ctx, s, tx, state, now); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
		SET status=?, last_error_code=?, lease_owner=NULL, lease_until=NULL, updated_at=?
		WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`), "parked", code, now, state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now)
	if err != nil {
		return fmt.Errorf("park calendar sync: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrCalendarSyncLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit park calendar sync: %w", err)
	}
	return nil
}

// ScheduleCalendarSync makes one parked, failed, or ready calendar due now.
// A live lease always wins; resetCursor may only invalidate an unleased state.
func (s *Store) ScheduleCalendarSync(ctx context.Context, calendarID string, now time.Time, resetCursor bool) error {
	if calendarID == "" {
		return ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
		SET status=?, next_sync_at=?, cursor=CASE WHEN ? THEN '' ELSE cursor END,
			generation=CASE WHEN ? THEN generation+1 ELSE generation END,
			last_error_code=NULL, lease_owner=NULL, lease_until=NULL, updated_at=?
		WHERE calendar_id=? AND (lease_until IS NULL OR lease_until<=?)
			AND EXISTS (SELECT 1 FROM calendars c JOIN connections conn ON conn.id=c.connection_id
				WHERE c.id=calendar_sync_state.calendar_id AND c.can_read=? AND conn.status=?)`), "pending", now, resetCursor, resetCursor, now, calendarID, now, true, "connected")
	if err != nil {
		return fmt.Errorf("schedule calendar sync: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var stateExists int
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM calendar_sync_state WHERE calendar_id=?`), calendarID).Scan(&stateExists); err != nil {
		return fmt.Errorf("check calendar sync state: %w", err)
	}
	if stateExists == 0 {
		return ErrNotFound
	}
	var eligible int
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM calendars c JOIN connections conn ON conn.id=c.connection_id
		WHERE c.id=? AND c.can_read=? AND conn.status=?`), calendarID, true, "connected").Scan(&eligible); err != nil {
		return fmt.Errorf("check calendar sync eligibility: %w", err)
	}
	if eligible == 0 {
		return ErrCalendarSyncIneligible
	}
	return ErrCalendarSyncActive
}

// ResetCalendarSync invalidates an opaque provider cursor after a provider
// reset response. The cursor clear and generation change are one CAS-protected
// transaction, so any page from the old cursor can no longer finish or sweep.
func (s *Store) ResetCalendarSync(ctx context.Context, state CalendarSyncState, now time.Time) (*CalendarSyncState, error) {
	if state.CalendarID == "" || state.LeaseOwner == "" {
		return nil, ErrCalendarSyncLeaseLost
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin calendar sync reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarSyncLease(ctx, s, tx, state, now); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
		SET cursor='', generation=generation+1, updated_at=?
		WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`), now, state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now)
	if err != nil {
		return nil, fmt.Errorf("reset calendar sync: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, ErrCalendarSyncLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit calendar sync reset: %w", err)
	}
	state.Cursor = ""
	state.Generation++
	return &state, nil
}

func (s *Store) ListCachedEvents(ctx context.Context, calendarIDs []string, start, end time.Time) ([]calendar.EventV2, []CachedSourceStatus, error) {
	if !end.After(start) {
		return nil, nil, errors.New("cached event range end must be after start")
	}
	if len(calendarIDs) == 0 {
		return []calendar.EventV2{}, []CachedSourceStatus{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(calendarIDs)), ",")
	args := make([]any, 0, len(calendarIDs)+2)
	for _, id := range calendarIDs {
		args = append(args, id)
	}
	allDayEnd := end.UTC()
	if allDayEnd.Hour() != 0 || allDayEnd.Minute() != 0 || allDayEnd.Second() != 0 || allDayEnd.Nanosecond() != 0 {
		allDayEnd = allDayEnd.AddDate(0, 0, 1)
	}
	eventArgs := append(append([]any{}, args...), true, "connected", false, end, start, allDayEnd.Format(calendar.DateLayout), start.UTC().Format(calendar.DateLayout))
	q := fmt.Sprintf(`SELECT e.payload_json FROM cached_events e
		JOIN calendars c ON c.id=e.calendar_id
		JOIN connections conn ON conn.id=c.connection_id
		WHERE e.calendar_id IN (%s) AND c.can_read=? AND conn.status=? AND e.deleted=? AND (
			(start_at IS NOT NULL AND start_at<? AND end_at>?) OR
			(start_date IS NOT NULL AND start_date<? AND end_date>?)
		) ORDER BY e.start_at IS NULL, e.start_at, e.start_date, e.event_id`, placeholders)
	rows, err := s.db.QueryContext(ctx, s.query(q), eventArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("list cached events: %w", err)
	}
	defer rows.Close()
	events := make([]calendar.EventV2, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, nil, fmt.Errorf("scan cached event: %w", err)
		}
		var event calendar.EventV2
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, nil, fmt.Errorf("decode cached event: %w", err)
		}
		// The browser contract is recurrence-expanded. Some adapters retain a
		// series master in the projection for reconciliation, but returning it
		// alongside its first expanded occurrence renders that occurrence twice.
		if event.InstanceKind == "seriesMaster" {
			continue
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	statusRows, err := s.db.QueryContext(ctx, s.query(fmt.Sprintf(`SELECT c.id, conn.provider, ss.status, ss.last_success_at, ss.last_error_code, ss.next_sync_at
		FROM calendars c JOIN connections conn ON conn.id=c.connection_id JOIN calendar_sync_state ss ON ss.calendar_id=c.id
		WHERE c.id IN (%s) AND c.can_read=? AND conn.status=? ORDER BY c.id`, placeholders)), append(args, true, "connected")...)
	if err != nil {
		return nil, nil, fmt.Errorf("list cached source status: %w", err)
	}
	defer statusRows.Close()
	sources := make([]CachedSourceStatus, 0, len(calendarIDs))
	for statusRows.Next() {
		var status CachedSourceStatus
		var lastSuccess sql.NullTime
		var errorCode sql.NullString
		var nextSync time.Time
		if err := statusRows.Scan(&status.CalendarID, &status.Provider, &status.Status, &lastSuccess, &errorCode, &nextSync); err != nil {
			return nil, nil, fmt.Errorf("scan cached source status: %w", err)
		}
		if lastSuccess.Valid {
			status.LastSuccessAt = &lastSuccess.Time
		}
		status.ErrorCode = errorCode.String
		status.Stale = status.LastSuccessAt == nil || nextSync.Before(time.Now().UTC())
		sources = append(sources, status)
	}
	if err := statusRows.Err(); err != nil {
		return nil, nil, err
	}
	return events, sources, nil
}

func (s *Store) UpsertCachedEvent(ctx context.Context, event calendar.EventV2, syncedAt time.Time) error {
	if event.CalendarID == "" {
		return errors.New("cached event calendar ID is required")
	}
	generation, err := s.calendarSyncGeneration(ctx, event.CalendarID)
	if err != nil {
		return err
	}
	if err := upsertCachedEvent(ctx, s, s.db, event, event.ID, generation, syncedAt); err != nil {
		return err
	}
	return nil
}

func (s *Store) DeleteCachedEvent(ctx context.Context, ref calendar.EventRef, syncedAt time.Time) error {
	if ref.CalendarID == "" || ref.EventID == "" {
		return errors.New("cached event reference requires calendar and event IDs")
	}
	generation, err := s.calendarSyncGeneration(ctx, ref.CalendarID)
	if err != nil {
		return err
	}
	return tombstoneCachedEvent(ctx, s, s.db, ref, generation, syncedAt)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) calendarSyncGeneration(ctx context.Context, calendarID string) (int64, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, s.query(`SELECT generation FROM calendar_sync_state WHERE calendar_id=?`), calendarID).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read calendar sync generation: %w", err)
	}
	return generation, nil
}

func upsertCachedEvent(ctx context.Context, s *Store, db execer, event calendar.EventV2, sourceObjectID string, generation int64, syncedAt time.Time) error {
	if event.ID == "" {
		return errors.New("cached event ID is required")
	}
	if err := calendar.ValidateEventTimeRangeV2(event.Start, event.End); err != nil {
		return fmt.Errorf("invalid cached event %q: %w", event.ID, err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode cached event: %w", err)
	}
	var startAt, endAt any
	var startDate, endDate any
	if event.Start.IsAllDay() {
		startDate, endDate = event.Start.Date, event.End.Date
	} else {
		startAt, err = event.Start.Instant()
		if err != nil {
			return fmt.Errorf("parse cached event start: %w", err)
		}
		endAt, err = event.End.Instant()
		if err != nil {
			return fmt.Errorf("parse cached event end: %w", err)
		}
	}
	q := `INSERT INTO cached_events(calendar_id, event_id, source_object_id, etag, payload_json, start_at, end_at,
		start_date, end_date, deleted, sync_generation, provider_updated_at, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(calendar_id, event_id) DO UPDATE SET source_object_id=excluded.source_object_id, etag=excluded.etag,
		payload_json=excluded.payload_json, start_at=excluded.start_at, end_at=excluded.end_at, start_date=excluded.start_date,
		end_date=excluded.end_date, deleted=excluded.deleted, sync_generation=excluded.sync_generation,
		provider_updated_at=excluded.provider_updated_at, synced_at=excluded.synced_at`
	if _, err := db.ExecContext(ctx, s.query(q), event.CalendarID, event.ID, sourceObjectID, event.ETag, string(payload), startAt, endAt,
		startDate, endDate, false, generation, event.Updated, syncedAt); err != nil {
		return fmt.Errorf("upsert cached event: %w", err)
	}
	return nil
}

func tombstoneCachedEvent(ctx context.Context, s *Store, db execer, ref calendar.EventRef, generation int64, syncedAt time.Time) error {
	q := `INSERT INTO cached_events(calendar_id, event_id, source_object_id, etag, payload_json, deleted, sync_generation, synced_at)
		VALUES (?, ?, '', '', ?, ?, ?, ?)
		ON CONFLICT(calendar_id, event_id) DO UPDATE SET deleted=excluded.deleted, sync_generation=excluded.sync_generation, synced_at=excluded.synced_at`
	if _, err := db.ExecContext(ctx, s.query(q), ref.CalendarID, ref.EventID, `{}`, true, generation, syncedAt); err != nil {
		return fmt.Errorf("tombstone cached event: %w", err)
	}
	return nil
}

func tombstoneMissingObjectMembers(ctx context.Context, s *Store, tx *sql.Tx, calendarID, sourceObjectID string, members map[string]struct{}, generation int64, syncedAt time.Time) error {
	rows, err := tx.QueryContext(ctx, s.query(`SELECT event_id FROM cached_events
		WHERE calendar_id=? AND source_object_id=? AND deleted=?`), calendarID, sourceObjectID, false)
	if err != nil {
		return fmt.Errorf("list cached object members: %w", err)
	}
	var missing []string
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan cached object member: %w", err)
		}
		if _, present := members[eventID]; !present {
			missing = append(missing, eventID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate cached object members: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, eventID := range missing {
		if err := tombstoneCachedEvent(ctx, s, tx, calendar.EventRef{CalendarID: calendarID, EventID: eventID}, generation, syncedAt); err != nil {
			return err
		}
	}
	return nil
}

func lockCalendarSyncLease(ctx context.Context, s *Store, tx *sql.Tx, state CalendarSyncState, now time.Time) error {
	q := `SELECT calendar_id FROM calendar_sync_state
		WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`
	if s.dialect == DialectPostgres {
		q += " FOR UPDATE"
	}
	var calendarID string
	err := tx.QueryRowContext(ctx, s.query(q), state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now).Scan(&calendarID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarSyncLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock calendar sync lease: %w", err)
	}
	if s.dialect == DialectSQLite {
		// A matched no-op write acquires SQLite's writer lock before pages can
		// touch projection rows, serializing this lease with a reclaim attempt.
		res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET updated_at=updated_at
			WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`), state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now)
		if err != nil {
			return fmt.Errorf("serialize calendar sync lease: %w", err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrCalendarSyncLeaseLost
		}
	}
	return nil
}

func selectCalendarSyncStateForUpdate(ctx context.Context, s *Store, tx *sql.Tx, calendarID string) (CalendarSyncState, error) {
	if s.dialect == DialectSQLite {
		// SQLite has no row-level FOR UPDATE. Taking a no-op writer lock before
		// reading prevents a concurrent lease completion from racing a window
		// reset decision in this transaction.
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET updated_at=updated_at WHERE calendar_id=?`), calendarID); err != nil {
			return CalendarSyncState{}, fmt.Errorf("serialize calendar sync state: %w", err)
		}
	}
	q := `SELECT calendar_id, strategy, cursor, window_start, window_end, generation, status, next_sync_at,
		last_started_at, last_success_at, last_error_code, lease_owner, lease_until
		FROM calendar_sync_state WHERE calendar_id=?`
	if s.dialect == DialectPostgres {
		q += " FOR UPDATE"
	}
	return scanCalendarSyncState(tx.QueryRowContext(ctx, s.query(q), calendarID))
}

func safeCalendarSyncCode(code string) bool {
	if len(code) == 0 || len(code) > 64 {
		return false
	}
	for _, value := range code {
		if (value < 'a' || value > 'z') && (value < '0' || value > '9') && value != '_' {
			return false
		}
	}
	return true
}

type scanner interface{ Scan(...any) error }

func scanCalendarSyncState(row scanner) (CalendarSyncState, error) {
	var state CalendarSyncState
	var started, success, until sql.NullTime
	var errorCode, owner sql.NullString
	if err := row.Scan(&state.CalendarID, &state.Strategy, &state.Cursor, &state.WindowStart, &state.WindowEnd,
		&state.Generation, &state.Status, &state.NextSyncAt, &started, &success, &errorCode, &owner, &until); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, ErrNotFound
		}
		return state, err
	}
	if started.Valid {
		state.LastStartedAt = &started.Time
	}
	if success.Valid {
		state.LastSuccessAt = &success.Time
	}
	if until.Valid {
		state.LeaseUntil = &until.Time
	}
	state.LastErrorCode, state.LeaseOwner = errorCode.String, owner.String
	return state, nil
}
