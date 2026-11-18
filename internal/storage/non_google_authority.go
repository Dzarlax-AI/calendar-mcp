package storage

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// CompleteNonGoogleAuthorization publishes a complete staged discovery only
// while the verification's original connection authority is still current.
func (s *Store) CompleteNonGoogleAuthorization(ctx context.Context, snapshot CredentialSnapshot, staged []Calendar, fingerprint, display string, now time.Time) error {
	if snapshot.Provider != "apple" && snapshot.Provider != "microsoft" {
		return ErrAuthorityChanged
	}
	seen := make(map[string]bool, len(staged))
	for _, c := range staged {
		if c.ConnectionID != snapshot.ConnectionID || c.ID == "" || c.ProviderCalendarID == "" {
			return ErrAuthorityChanged
		}
		if _, duplicate := seen[c.ProviderCalendarID]; duplicate {
			return ErrAuthorityChanged
		}
		seen[c.ProviderCalendarID] = c.CanRead
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET status='connected',last_verified_at=?,last_error_code=NULL,updated_at=?,account_fingerprint=?,display_name=? WHERE id=? AND provider=? AND authorization_revision=? AND status=?`), now, now, fingerprint, display, snapshot.ConnectionID, snapshot.Provider, snapshot.AuthorizationRevision, snapshot.Status)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAuthorityChanged
	}
	// Only absent sources lose their proof. Existing readable siblings keep theirs.
	rows, err := tx.QueryContext(ctx, s.query(`SELECT id,provider_calendar_id,can_read FROM calendars WHERE connection_id=?`), snapshot.ConnectionID)
	if err != nil {
		return err
	}
	var absent []string
	for rows.Next() {
		var id, providerID string
		var readable bool
		if err = rows.Scan(&id, &providerID, &readable); err != nil {
			rows.Close()
			return err
		}
		if readable && !seen[providerID] {
			absent = append(absent, id)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range absent {
		if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET can_read=?,can_write=?,detail_receipt=? WHERE id=?`), false, false, uuid.NewString(), id); err != nil {
			return err
		}
		if err = s.purgeNonGoogleSource(ctx, tx, id, now); err != nil {
			return err
		}
	}
	for _, c := range staged {
		if err = upsertCalendarWithAuthority(ctx, s, tx, c); err != nil {
			return err
		}
	}
	// Legacy permission failures may predate projection purging. Invalidate
	// them before clearing the error, within the same authority transaction.
	rows, err = tx.QueryContext(ctx, s.query(s.eventSyncTimeComparisons(`SELECT calendar_id FROM calendar_sync_state
		WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=? AND can_read=? AND enabled=TRUE)
		AND last_error_code='permission' AND (lease_until IS NULL OR lease_until<=?)`)), snapshot.ConnectionID, true, now)
	if err != nil {
		return err
	}
	var denied []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		denied = append(denied, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range denied {
		if err = s.purgeAuthorizedProjection(ctx, tx, projectionCalendarScope, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET detail_receipt=? WHERE id=?`), uuid.NewString(), id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, s.query(s.eventSyncTimeComparisons(`UPDATE calendar_sync_state SET status='pending',next_sync_at=?,last_error_code=NULL,updated_at=?,
		cursor=CASE WHEN last_error_code='permission' THEN '' ELSE cursor END,
		generation=CASE WHEN last_error_code='permission' THEN generation+1 ELSE generation END,
		last_success_at=CASE WHEN last_error_code IN ('auth','permission') THEN NULL ELSE last_success_at END
		WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=? AND can_read=? AND enabled=TRUE) AND (status='parked' OR last_error_code='permission') AND (lease_until IS NULL OR lease_until<=?)`)), now, now, snapshot.ConnectionID, true, now); err != nil {
		return err
	}
	return tx.Commit()
}

// purgeNonGoogleSource fences stale workers and removes projection made under a previous grant.
func (s *Store) purgeNonGoogleSource(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,cursor='',status='pending',lease_owner=NULL,lease_until=NULL,last_success_at=NULL,last_error_code=NULL,next_sync_at=?,updated_at=? WHERE calendar_id=?`), now, now, id); err != nil {
		return err
	}
	for _, table := range []string{"cached_events", "calendar_sync_objects", "calendar_sync_quarantine"} {
		if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM `+table+` WHERE calendar_id=?`), id); err != nil {
			return err
		}
	}
	return nil
}
