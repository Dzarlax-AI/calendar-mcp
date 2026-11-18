package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"calendar-mcp/internal/calendar"
)

// RepairCalendarTimeZone returns the stored source calendar timezone; callers
// must validate it before using it to interpret civil dates.
func (s *Store) RepairCalendarTimeZone(ctx context.Context, calendarID string) (string, error) {
	if calendarID == "" {
		return "", errors.New("calendar ID is required")
	}
	var zone string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT timezone FROM calendars WHERE id=?`), calendarID).Scan(&zone)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read repair calendar timezone: %w", err)
	}
	return zone, nil
}

// AppleCalendarTimezoneUpdate carries the source metadata captured before CalDAV I/O.
type AppleCalendarTimezoneUpdate struct {
	Before   Calendar
	Timezone string
}

// UpdateAppleCalendarTimezones applies a metadata-only change after rechecking the
// exact connection, source grant and prior timezone. Any stale row aborts the batch.
func (s *Store) UpdateAppleCalendarTimezones(ctx context.Context, snapshot CredentialSnapshot, updates []AppleCalendarTimezoneUpdate) (int, error) {
	if snapshot.Provider != "apple" || snapshot.Status != "connected" || len(updates) == 0 || len(updates) > 16 {
		return 0, ErrAuthorityChanged
	}
	for _, update := range updates {
		c := update.Before
		if c.ConnectionID != snapshot.ConnectionID || c.ProviderCalendarID == "" ||
			c.ID != calendar.AccountCalendarID("apple", c.ConnectionID, c.ProviderCalendarID) ||
			!c.Enabled || !c.CanRead || safeDiscoveryTimezone("apple", update.Timezone) == "" {
			return 0, ErrAuthorityChanged
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockCalendarConnection(ctx, s, tx, updates[0].Before.ID); err != nil {
		return 0, err
	}
	var valid int
	err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM connections
		WHERE id=? AND provider='apple' AND status='connected' AND authorization_revision=?
		AND credential_version=? AND encrypted_credentials=?`), snapshot.ConnectionID, snapshot.AuthorizationRevision,
		snapshot.CredentialVersion, snapshot.EncryptedCredentials).Scan(&valid)
	if err != nil {
		return 0, err
	}
	if valid != 1 {
		return 0, ErrAuthorityChanged
	}
	changed := 0
	for _, update := range updates {
		c := update.Before
		err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM calendars cal
			WHERE cal.id=? AND cal.connection_id=? AND cal.provider_calendar_id=?
			AND cal.enabled=TRUE AND cal.can_read=TRUE AND cal.detail_receipt=? AND cal.timezone=?
			AND NOT EXISTS (SELECT 1 FROM calendar_sync_state ss WHERE ss.calendar_id=cal.id
				AND ss.last_error_code IN ('auth','permission'))`), c.ID, c.ConnectionID, c.ProviderCalendarID, c.DetailReceipt, c.Timezone).Scan(&valid)
		if err != nil {
			return 0, err
		}
		if valid != 1 {
			return 0, ErrAuthorityChanged
		}
		if c.Timezone != update.Timezone {
			if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=? WHERE id=?`), update.Timezone, c.ID); err != nil {
				return 0, err
			}
			changed++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}
