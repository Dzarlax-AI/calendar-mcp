package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

var ErrCalendarSyncLeaseLost = errors.New("calendar sync lease is no longer owned by this worker")

// ListDueEventSyncQuarantine returns at most limit active repairable objects
// for a calendar already owned by state. The caller must still apply its
// result through the lease-protected mutation method below.
func (s *Store) ListDueEventSyncQuarantine(ctx context.Context, state CalendarSyncState, now time.Time, limit int) ([]CalendarSyncQuarantine, error) {
	if state.CalendarID == "" || state.LeaseOwner == "" {
		return nil, ErrCalendarSyncLeaseLost
	}
	if limit <= 0 {
		return []CalendarSyncQuarantine{}, nil
	}
	q := `SELECT calendar_id, object_id, etag, error_code, first_seen_at, last_seen_at, next_repair_at, repair_attempts, active, provider_mutation_authorized_etag
		FROM calendar_sync_quarantine WHERE calendar_id=? AND active=? AND next_repair_at<=?
		ORDER BY next_repair_at, object_id LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.query(q), state.CalendarID, true, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due event sync quarantine: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]CalendarSyncQuarantine, 0, limit)
	for rows.Next() {
		var item CalendarSyncQuarantine
		if err := rows.Scan(&item.CalendarID, &item.ObjectID, &item.ETag, &item.ErrorCode, &item.FirstSeenAt, &item.LastSeenAt, &item.NextRepairAt, &item.RepairAttempts, &item.Active, &item.ProviderMutationAuthorizedETag); err != nil {
			return nil, fmt.Errorf("scan due event sync quarantine: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due event sync quarantine: %w", err)
	}
	return items, nil
}

// ListActiveEventSyncQuarantineDiagnostics returns a bounded, paginated
// operator view. The raw payload is deliberately excluded; callers must use
// GetRawEventSyncArtifact with the exact object identity after authorization.
func (s *Store) ListActiveEventSyncQuarantineDiagnostics(ctx context.Context, limit, offset int) ([]EventSyncQuarantineDiagnostic, error) {
	if limit <= 0 {
		return []EventSyncQuarantineDiagnostic{}, nil
	}
	if offset < 0 {
		return nil, errors.New("event sync quarantine offset must not be negative")
	}
	q := `SELECT q.calendar_id, q.object_id, q.etag, q.error_code, q.first_seen_at, q.last_seen_at, q.next_repair_at, q.repair_attempts, q.active,
		c.name, conn.provider, s.status, s.last_success_at, s.last_error_code,
		a.etag, a.payload_sha256, a.content_type, a.provider_status, a.provider_reason, a.truncated, a.captured_at, a.expires_at
		FROM calendar_sync_quarantine q
		JOIN calendars c ON c.id=q.calendar_id
		JOIN connections conn ON conn.id=c.connection_id
		LEFT JOIN calendar_sync_state s ON s.calendar_id=q.calendar_id
		LEFT JOIN calendar_sync_raw_artifacts a ON a.calendar_id=q.calendar_id AND a.object_id=q.object_id AND a.etag=q.etag AND a.expires_at>?
		WHERE q.active=?
		ORDER BY q.last_seen_at DESC, q.calendar_id, q.object_id LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, s.query(q), time.Now().UTC(), true, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list active event sync quarantine diagnostics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]EventSyncQuarantineDiagnostic, 0, limit)
	for rows.Next() {
		item, err := scanEventSyncQuarantineDiagnostic(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active event sync quarantine diagnostics: %w", err)
	}
	return items, nil
}

// GetActiveEventSyncQuarantineDiagnostic returns one active object plus safe
// artifact metadata. It never decrypts the artifact payload.
func (s *Store) GetActiveEventSyncQuarantineDiagnostic(ctx context.Context, calendarID, objectID string) (*EventSyncQuarantineDiagnostic, error) {
	if calendarID == "" || objectID == "" {
		return nil, ErrNotFound
	}
	q := `SELECT q.calendar_id, q.object_id, q.etag, q.error_code, q.first_seen_at, q.last_seen_at, q.next_repair_at, q.repair_attempts, q.active,
		c.name, conn.provider, s.status, s.last_success_at, s.last_error_code,
		a.etag, a.payload_sha256, a.content_type, a.provider_status, a.provider_reason, a.truncated, a.captured_at, a.expires_at
		FROM calendar_sync_quarantine q
		JOIN calendars c ON c.id=q.calendar_id
		JOIN connections conn ON conn.id=c.connection_id
		LEFT JOIN calendar_sync_state s ON s.calendar_id=q.calendar_id
		LEFT JOIN calendar_sync_raw_artifacts a ON a.calendar_id=q.calendar_id AND a.object_id=q.object_id AND a.etag=q.etag AND a.expires_at>?
		WHERE q.calendar_id=? AND q.object_id=? AND q.active=?`
	item, err := scanEventSyncQuarantineDiagnostic(s.db.QueryRowContext(ctx, s.query(q), time.Now().UTC(), calendarID, objectID, true))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get active event sync quarantine diagnostic: %w", err)
	}
	return &item, nil
}

// ListRecentEventSyncProviderCorrections returns the bounded operator audit
// trail for repairs that changed provider data and then passed revalidation.
// It deliberately does not join quarantine: corrected objects are resolved.
func (s *Store) ListRecentEventSyncProviderCorrections(ctx context.Context, limit int) ([]EventSyncProviderCorrection, error) {
	if limit <= 0 {
		return []EventSyncProviderCorrection{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT p.calendar_id, p.object_id, p.outcome, p.corrected_at, c.name, conn.provider
		FROM calendar_sync_provider_corrections p
		JOIN calendars c ON c.id=p.calendar_id
		JOIN connections conn ON conn.id=c.connection_id
		ORDER BY p.corrected_at DESC, p.calendar_id, p.object_id LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("list recent event sync provider corrections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]EventSyncProviderCorrection, 0, limit)
	for rows.Next() {
		var item EventSyncProviderCorrection
		if err := rows.Scan(&item.CalendarID, &item.ObjectID, &item.Outcome, &item.CorrectedAt, &item.CalendarName, &item.Provider); err != nil {
			return nil, fmt.Errorf("scan recent event sync provider correction: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent event sync provider corrections: %w", err)
	}
	return items, nil
}

// ScheduleEventSyncObjectRepair makes one active object due now and wakes an
// unleased calendar so the worker can claim it immediately. A row that is
// already due is left untouched, making repeated operator requests harmless.
func (s *Store) ScheduleEventSyncObjectRepair(ctx context.Context, calendarID, objectID, expectedETag string, now time.Time) (bool, error) {
	if calendarID == "" || objectID == "" || expectedETag == "" {
		return false, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin schedule event sync object repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The diagnostics handler is the authenticated operator boundary. Bind the
	// grant to the current ETag so a later provider version needs a new click.
	authorization, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_quarantine
		SET provider_mutation_authorized_etag=etag
		WHERE calendar_id=? AND object_id=? AND active=? AND etag=?`), calendarID, objectID, true, expectedETag)
	if err != nil {
		return false, fmt.Errorf("authorize event sync object repair: %w", err)
	}
	authorized, err := authorization.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read event sync object repair authorization: %w", err)
	}
	if authorized == 0 {
		var active bool
		if err := tx.QueryRowContext(ctx, s.query(`SELECT active FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?`), calendarID, objectID).Scan(&active); errors.Is(err, sql.ErrNoRows) || !active {
			return false, ErrNotFound
		} else if err != nil {
			return false, fmt.Errorf("read event sync object repair authorization: %w", err)
		}
		return false, ErrEventSyncRepairETagMismatch
	}
	result, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_quarantine SET next_repair_at=?
		WHERE calendar_id=? AND object_id=? AND active=? AND etag=? AND next_repair_at>?`), now, calendarID, objectID, true, expectedETag, now)
	if err != nil {
		return false, fmt.Errorf("schedule event sync object repair: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read scheduled event sync object repair: %w", err)
	}
	var active bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT active FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?`), calendarID, objectID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) || !active {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read event sync object repair: %w", err)
	}
	// A parked/future calendar is otherwise invisible to ClaimDueCalendarSync.
	// Do not disturb an active lease; the current worker owns its schedule.
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET next_sync_at=?, updated_at=?
		WHERE calendar_id=? AND next_sync_at>? AND (lease_owner IS NULL OR lease_until IS NULL OR lease_until<=?)`), now, now, calendarID, now, now); err != nil {
		return false, fmt.Errorf("wake calendar for event sync repair: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit event sync object repair: %w", err)
	}
	return changed == 1, nil
}

// ConsumeEventSyncProviderMutationAuthorization atomically spends the
// operator's ETag-bound authorization while the worker still owns the
// calendar lease. A process crash after this point requires a fresh explicit
// operator action instead of risking an unapproved provider mutation retry.
func (s *Store) ConsumeEventSyncProviderMutationAuthorization(ctx context.Context, state CalendarSyncState, calendarID, objectID, etag string, now time.Time) (bool, error) {
	if state.CalendarID == "" || state.LeaseOwner == "" || state.CalendarID != calendarID || objectID == "" || etag == "" {
		return false, ErrCalendarSyncLeaseLost
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin consume event sync repair authorization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarSyncLease(ctx, s, tx, state, now); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_quarantine SET provider_mutation_authorized_etag=''
		WHERE calendar_id=? AND object_id=? AND active=? AND etag=? AND provider_mutation_authorized_etag=?`), calendarID, objectID, true, etag, etag)
	if err != nil {
		return false, fmt.Errorf("consume event sync repair authorization: %w", err)
	}
	consumed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read consumed event sync repair authorization: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit event sync repair authorization: %w", err)
	}
	return consumed == 1, nil
}

func scanEventSyncQuarantineDiagnostic(row scanner) (EventSyncQuarantineDiagnostic, error) {
	var item EventSyncQuarantineDiagnostic
	var lastSuccess sql.NullTime
	var syncStatus, lastError sql.NullString
	var artifactETag, hash, contentType, providerReason sql.NullString
	var providerStatus sql.NullInt64
	var truncated sql.NullBool
	var captured, expires sql.NullTime
	if err := row.Scan(&item.CalendarID, &item.ObjectID, &item.ETag, &item.ErrorCode, &item.FirstSeenAt, &item.LastSeenAt, &item.NextRepairAt, &item.RepairAttempts, &item.Active,
		&item.CalendarName, &item.Provider, &syncStatus, &lastSuccess, &lastError,
		&artifactETag, &hash, &contentType, &providerStatus, &providerReason, &truncated, &captured, &expires); err != nil {
		return item, err
	}
	item.SyncStatus, item.LastErrorCode = syncStatus.String, lastError.String
	if lastSuccess.Valid {
		item.LastSuccessAt = &lastSuccess.Time
	}
	if artifactETag.Valid {
		item.Artifact = &RawEventSyncArtifactMetadata{ETag: artifactETag.String, PayloadSHA256: hash.String, ContentType: contentType.String, ProviderStatus: int(providerStatus.Int64), ProviderReason: providerReason.String, Truncated: truncated.Bool, CapturedAt: captured.Time, ExpiresAt: expires.Time}
	}
	return item, nil
}

// ApplyEventSyncObjectRepair applies a single repair outcome while the same
// calendar lease remains current. It never changes the opaque main cursor.
func (s *Store) ApplyEventSyncObjectRepair(ctx context.Context, state CalendarSyncState, batch EventSyncRepairBatch, now time.Time) error {
	if state.CalendarID == "" || state.LeaseOwner == "" || batch.ObjectID == "" {
		return ErrCalendarSyncLeaseLost
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin event sync object repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarSyncLease(ctx, s, tx, state, now); err != nil {
		return err
	}
	switch batch.Outcome {
	case calendar.EventSyncObjectReplaceMembership, calendar.EventSyncObjectProviderCorrected:
		if len(batch.Upserts) == 0 {
			return errors.New("repair replacement requires event membership")
		}
		members := make(map[string]struct{}, len(batch.Upserts))
		for _, upsert := range batch.Upserts {
			if upsert.SourceObjectID != "" && upsert.SourceObjectID != batch.ObjectID {
				return errors.New("repair membership object does not match quarantine object")
			}
			members[upsert.Event.ID] = struct{}{}
		}
		if err := tombstoneMissingObjectMembers(ctx, s, tx, state.CalendarID, batch.ObjectID, members, state.Generation, now); err != nil {
			return err
		}
		for _, upsert := range batch.Upserts {
			event := upsert.Event
			event.CalendarID = state.CalendarID
			if err := upsertCachedEvent(ctx, s, tx, event, batch.ObjectID, state.Generation, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO calendar_sync_objects(calendar_id, object_id, etag, sync_generation)
			VALUES (?, ?, ?, ?) ON CONFLICT(calendar_id, object_id) DO UPDATE SET etag=excluded.etag, sync_generation=excluded.sync_generation`), state.CalendarID, batch.ObjectID, batch.ETag, state.Generation); err != nil {
			return fmt.Errorf("upsert repaired calendar sync object: %w", err)
		}
		if err := clearEventSyncQuarantine(ctx, s, tx, state.CalendarID, batch.ObjectID); err != nil {
			return err
		}
		if batch.Outcome == calendar.EventSyncObjectProviderCorrected {
			if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO calendar_sync_provider_corrections(calendar_id, object_id, outcome, corrected_at)
				VALUES (?, ?, ?, ?)`), state.CalendarID, batch.ObjectID, string(batch.Outcome), now); err != nil {
				return fmt.Errorf("record provider correction: %w", err)
			}
		}
	case calendar.EventSyncObjectAbsentFromProjection, calendar.EventSyncObjectProviderDeleted:
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE cached_events SET deleted=?, sync_generation=?, synced_at=?
			WHERE calendar_id=? AND source_object_id=? AND deleted=?`), true, state.Generation, now, state.CalendarID, batch.ObjectID, false); err != nil {
			return fmt.Errorf("tombstone repaired object membership: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM calendar_sync_objects WHERE calendar_id=? AND object_id=?`), state.CalendarID, batch.ObjectID); err != nil {
			return fmt.Errorf("delete repaired calendar sync object: %w", err)
		}
		if err := clearEventSyncQuarantine(ctx, s, tx, state.CalendarID, batch.ObjectID); err != nil {
			return err
		}
	case calendar.EventSyncObjectStillQuarantined:
		if batch.Warning == nil || batch.Warning.ObjectID != batch.ObjectID || batch.Warning.ErrorCode != string(calendar.EventSyncProtocol) {
			return errors.New("repair quarantine result requires matching protocol warning")
		}
		if err := upsertEventSyncWarning(ctx, s, tx, state.CalendarID, *batch.Warning, now); err != nil {
			return err
		}
		if batch.NextRepairAt != nil {
			if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_quarantine SET next_repair_at=?, repair_attempts=repair_attempts+1
				WHERE calendar_id=? AND object_id=? AND active=?`), *batch.NextRepairAt, state.CalendarID, batch.ObjectID, true); err != nil {
				return fmt.Errorf("reschedule quarantined repair: %w", err)
			}
		}
	default:
		return errors.New("unknown event sync repair outcome")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit event sync object repair: %w", err)
	}
	return nil
}

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
		WHERE ss.status IN (?, ?, ?, ?, ?) AND conn.status=? AND c.can_read=? AND ss.next_sync_at<=? AND (ss.lease_until IS NULL OR ss.lease_until<=?)
		ORDER BY ss.next_sync_at, ss.calendar_id LIMIT 1`
	if s.dialect == DialectPostgres {
		q += " FOR UPDATE SKIP LOCKED"
	}
	state, err := scanCalendarSyncState(tx.QueryRowContext(ctx, s.query(q), "pending", "ready", "failed", "degraded", "syncing", "connected", true, now, now))
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
	degradedCode := ""
	if batch.Degraded {
		degradedCode = batch.ErrorCode
		if degradedCode == "" {
			degradedCode = string(calendar.EventSyncProtocol)
		}
		if degradedCode != string(calendar.EventSyncProtocol) || !safeCalendarSyncCode(degradedCode) {
			return ErrInvalidSyncCode
		}
	}
	for _, warning := range batch.Warnings {
		if warning.ObjectID == "" || warning.ErrorCode != string(calendar.EventSyncProtocol) {
			return ErrInvalidSyncCode
		}
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
		if objectID == "" {
			return errors.New("deleted source object ID is required")
		}
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
	// Warnings are written before successful data. A valid upsert/delete for
	// the same object below clears it in this transaction, so stale warnings
	// can never win over confirmed provider state.
	for _, warning := range batch.Warnings {
		if err := upsertEventSyncWarning(ctx, s, tx, state.CalendarID, warning, syncedAt); err != nil {
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
		if err := clearEventSyncQuarantine(ctx, s, tx, state.CalendarID, sourceObjectID); err != nil {
			return err
		}
	}
	for _, eventID := range batch.DeletedEventIDs {
		if err := tombstoneCachedEvent(ctx, s, tx, calendar.EventRef{CalendarID: state.CalendarID, EventID: eventID}, state.Generation, syncedAt); err != nil {
			return err
		}
		if err := clearEventSyncQuarantine(ctx, s, tx, state.CalendarID, eventID); err != nil {
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
		if err := clearEventSyncQuarantine(ctx, s, tx, state.CalendarID, objectID); err != nil {
			return err
		}
	}
	if final {
		var activeQuarantine bool
		if err := tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(
			SELECT 1 FROM calendar_sync_quarantine WHERE calendar_id=? AND active=?)`), state.CalendarID, true).Scan(&activeQuarantine); err != nil {
			return fmt.Errorf("check active event sync quarantine: %w", err)
		}
		finalDegraded := batch.Degraded || activeQuarantine
		if finalDegraded && degradedCode == "" {
			degradedCode = string(calendar.EventSyncProtocol)
		}
		nextSyncAt := state.NextSyncAt
		if batch.NextSyncAt != nil {
			nextSyncAt = *batch.NextSyncAt
		}
		if batch.FullSync {
			// A malformed object can be absent from the valid replacement pages;
			// do not delete its last known membership or inventory until repair
			// resolves it. The active quarantine table is the authoritative
			// exclusion set, including warnings committed on earlier pages.
			if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM cached_events
				WHERE calendar_id=? AND sync_generation<>? AND NOT EXISTS (
					SELECT 1 FROM calendar_sync_quarantine q WHERE q.calendar_id=cached_events.calendar_id
						AND q.object_id=cached_events.source_object_id AND q.active=?
				)`), state.CalendarID, state.Generation, true); err != nil {
				return fmt.Errorf("sweep cached event generation: %w", err)
			}
			if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM calendar_sync_objects
				WHERE calendar_id=? AND sync_generation<>? AND NOT EXISTS (
					SELECT 1 FROM calendar_sync_quarantine q WHERE q.calendar_id=calendar_sync_objects.calendar_id
						AND q.object_id=calendar_sync_objects.object_id AND q.active=?
				)`), state.CalendarID, state.Generation, true); err != nil {
				return fmt.Errorf("sweep calendar sync object generation: %w", err)
			}
		}
		if finalDegraded {
			res, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
				SET cursor=?, status=?, next_sync_at=?, last_success_at=?, last_error_code=?, lease_owner=NULL, lease_until=NULL, updated_at=?
				WHERE calendar_id=? AND generation=? AND status=? AND lease_owner=? AND lease_until>?`), state.Cursor, "degraded", nextSyncAt, syncedAt, degradedCode, syncedAt, state.CalendarID, state.Generation, "syncing", state.LeaseOwner, now)
			if err != nil {
				return fmt.Errorf("degrade calendar sync: %w", err)
			}
			changed, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return ErrCalendarSyncLeaseLost
			}
		} else {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert cached event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := selectCalendarSyncStateForUpdate(ctx, s, tx, event.CalendarID)
	if err != nil {
		return err
	}
	if err := upsertCachedEvent(ctx, s, tx, event, event.ID, state.Generation, syncedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert cached event: %w", err)
	}
	return nil
}

func (s *Store) DeleteCachedEvent(ctx context.Context, ref calendar.EventRef, syncedAt time.Time) error {
	if ref.CalendarID == "" || ref.EventID == "" {
		return errors.New("cached event reference requires calendar and event IDs")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete cached event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := selectCalendarSyncStateForUpdate(ctx, s, tx, ref.CalendarID)
	if err != nil {
		return err
	}
	if err := tombstoneCachedEvent(ctx, s, tx, ref, state.Generation, syncedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete cached event: %w", err)
	}
	return nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
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
	if _, err := db.ExecContext(ctx, s.query(q), event.CalendarID, event.ID, sourceObjectID, event.ETag, payload, startAt, endAt,
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

func upsertEventSyncWarning(ctx context.Context, s *Store, tx *sql.Tx, calendarID string, warning EventSyncWarning, now time.Time) error {
	if warning.ObjectID == "" || warning.ErrorCode != string(calendar.EventSyncProtocol) {
		return ErrInvalidSyncCode
	}
	// Active rows have no expiry. A changed ETag (or a previously resolved row
	// becoming active again) resets repair backoff; identical observations keep
	// the existing schedule and attempt count.
	if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO calendar_sync_quarantine
		(calendar_id, object_id, etag, error_code, first_seen_at, last_seen_at, next_repair_at, repair_attempts, active, provider_mutation_authorized_etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, '')
		ON CONFLICT(calendar_id, object_id) DO UPDATE SET
			etag=excluded.etag, error_code=excluded.error_code, last_seen_at=excluded.last_seen_at,
			first_seen_at=CASE WHEN NOT calendar_sync_quarantine.active THEN excluded.first_seen_at ELSE calendar_sync_quarantine.first_seen_at END,
			next_repair_at=CASE WHEN NOT calendar_sync_quarantine.active OR calendar_sync_quarantine.etag<>excluded.etag THEN excluded.next_repair_at ELSE calendar_sync_quarantine.next_repair_at END,
			repair_attempts=CASE WHEN NOT calendar_sync_quarantine.active OR calendar_sync_quarantine.etag<>excluded.etag THEN 0 ELSE calendar_sync_quarantine.repair_attempts END,
			active=excluded.active,
			provider_mutation_authorized_etag=CASE WHEN NOT calendar_sync_quarantine.active OR calendar_sync_quarantine.etag<>excluded.etag THEN '' ELSE calendar_sync_quarantine.provider_mutation_authorized_etag END`), calendarID, warning.ObjectID, warning.ETag, warning.ErrorCode, now, now, now, true); err != nil {
		return fmt.Errorf("upsert event sync quarantine: %w", err)
	}
	if warning.Diagnostic != nil && s.artifactCipher != nil {
		if err := upsertRawEventArtifact(ctx, s, tx, calendarID, warning.ObjectID, warning.ETag, *warning.Diagnostic, now); err != nil {
			return err
		}
	}
	return nil
}

func upsertRawEventArtifact(ctx context.Context, s *Store, tx *sql.Tx, calendarID, objectID, etag string, diagnostic calendar.EventSyncDiagnostic, now time.Time) error {
	payload := diagnostic.RawPayload
	truncated := diagnostic.Truncated
	if len(payload) > calendar.MaxEventSyncDiagnosticBytes {
		payload = payload[:calendar.MaxEventSyncDiagnosticBytes]
		truncated = true
	}
	hash := sha256.Sum256(diagnostic.RawPayload)
	encrypted, err := s.artifactCipher.Encrypt(payload, []byte("calendar-sync-artifact\x00"+calendarID+"\x00"+objectID+"\x00"+etag))
	if err != nil {
		return fmt.Errorf("encrypt raw event artifact: %w", err)
	}
	expires := now.Add(7 * 24 * time.Hour)
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO calendar_sync_raw_artifacts
		(calendar_id, object_id, etag, payload_ciphertext, payload_sha256, content_type, provider_status, provider_reason, truncated, captured_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(calendar_id, object_id) DO UPDATE SET etag=excluded.etag, payload_ciphertext=excluded.payload_ciphertext,
		payload_sha256=excluded.payload_sha256, content_type=excluded.content_type, provider_status=excluded.provider_status,
		provider_reason=excluded.provider_reason, truncated=excluded.truncated, captured_at=excluded.captured_at, expires_at=excluded.expires_at`),
		calendarID, objectID, etag, encrypted, hex.EncodeToString(hash[:]), diagnostic.ContentType, diagnostic.ProviderStatus, diagnostic.ProviderReason, truncated, now, expires)
	if err != nil {
		return fmt.Errorf("upsert raw event artifact: %w", err)
	}
	return nil
}

// GetRawEventSyncArtifact is an operator-only diagnostic read. The database
// stores ciphertext; decryption happens only after the caller supplies the
// exact calendar/object identity used as AES-GCM associated data.
func (s *Store) GetRawEventSyncArtifact(ctx context.Context, calendarID, objectID, expectedETag string) (*RawEventSyncArtifact, error) {
	if s.artifactCipher == nil || calendarID == "" || objectID == "" {
		return nil, ErrNotFound
	}
	if expectedETag == "" {
		return nil, ErrNotFound
	}
	var artifact RawEventSyncArtifact
	var encrypted []byte
	var truncated bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT a.calendar_id, a.object_id, a.etag, a.payload_ciphertext, a.payload_sha256, a.content_type, a.provider_status, a.provider_reason, a.truncated, a.captured_at, a.expires_at
		FROM calendar_sync_raw_artifacts a JOIN calendar_sync_quarantine q ON q.calendar_id=a.calendar_id AND q.object_id=a.object_id AND q.etag=a.etag
		WHERE a.calendar_id=? AND a.object_id=? AND q.active=? AND a.etag=? AND a.expires_at>? ORDER BY a.captured_at DESC LIMIT 1`), calendarID, objectID, true, expectedETag, time.Now().UTC()).Scan(
		&artifact.CalendarID, &artifact.ObjectID, &artifact.ETag, &encrypted, &artifact.PayloadSHA256, &artifact.ContentType, &artifact.ProviderStatus, &artifact.ProviderReason, &truncated, &artifact.CapturedAt, &artifact.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read raw event artifact: %w", err)
	}
	decoded, err := s.artifactCipher.Decrypt(encrypted, []byte("calendar-sync-artifact\x00"+calendarID+"\x00"+objectID+"\x00"+artifact.ETag))
	if err != nil {
		return nil, fmt.Errorf("decrypt raw event artifact: %w", err)
	}
	artifact.RawPayload, artifact.Truncated = decoded, truncated
	return &artifact, nil
}

func (s *Store) DeleteExpiredRawEventArtifacts(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, s.query(`DELETE FROM calendar_sync_raw_artifacts WHERE expires_at<=?`), now)
	if err != nil {
		return fmt.Errorf("delete expired raw event artifacts: %w", err)
	}
	return nil
}

func clearEventSyncQuarantine(ctx context.Context, s *Store, tx *sql.Tx, calendarID, objectID string) error {
	if objectID == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_quarantine SET active=?
		WHERE calendar_id=? AND object_id=? AND active=?`), false, calendarID, objectID, true); err != nil {
		return fmt.Errorf("resolve event sync quarantine: %w", err)
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
