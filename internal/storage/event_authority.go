package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"calendar-mcp/internal/calendar"
)

// ErrAuthorityChanged means that an operation's authorization snapshot is stale.
var ErrAuthorityChanged = errors.New("calendar authority changed")

// ErrAuthorityClosed distinguishes a committed source closure from a stale observation.
var ErrAuthorityClosed = errors.New("calendar authority closed")

// CredentialSnapshot is an immutable provider construction input. Ciphertext is
// deliberately excluded from page tickets and cursors.
type CredentialSnapshot struct {
	ConnectionID, Provider, AuthorizationRevision, Status string
	EncryptedCredentials                                  []byte
	CredentialVersion                                     int
}

// AuthorityTicket captures the source proof against which a page or a writer
// must be checked. Empty revisions or receipts never authorize cached details.
type AuthorityTicket struct {
	ConnectionID, CalendarID, AuthorizationRevision, DetailReceipt string
	Generation                                                     int64
}

// CredentialSnapshot reads only bounded encrypted credentials.
func (s *Store) CredentialSnapshot(ctx context.Context, id string) (CredentialSnapshot, error) {
	var v CredentialSnapshot
	err := s.db.QueryRowContext(ctx, s.query(`SELECT id, provider, authorization_revision, status, encrypted_credentials, credential_version
 FROM connections WHERE id=? AND credential_is_bounded`), id).Scan(&v.ConnectionID, &v.Provider, &v.AuthorizationRevision, &v.Status, &v.EncryptedCredentials, &v.CredentialVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, fmt.Errorf("read credential snapshot: %w", err)
	}
	return v, nil
}

// ValidateCredentialSnapshot rejects a superseded token even if no refresh is needed.
func (s *Store) ValidateCredentialSnapshot(ctx context.Context, snapshot CredentialSnapshot) error {
	var n int
	err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM connections WHERE id=? AND provider=? AND authorization_revision=? AND encrypted_credentials=?`), snapshot.ConnectionID, snapshot.Provider, snapshot.AuthorizationRevision, snapshot.EncryptedCredentials).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAuthorityChanged
	}
	return nil
}

// CompareAndSwapCredentials persists a refresh only for its original epoch and
// ciphertext. It does not change the authorization epoch or the receipt.
func (s *Store) CompareAndSwapCredentials(ctx context.Context, snapshot CredentialSnapshot, replacement []byte, updatedAt time.Time) error {
	if len(snapshot.EncryptedCredentials) > 16384 || len(replacement) > 16384 {
		return ErrAuthorityChanged
	}
	result, err := s.db.ExecContext(ctx, s.query(`UPDATE connections SET encrypted_credentials=?, updated_at=? WHERE id=? AND provider=? AND authorization_revision=? AND encrypted_credentials=?`), replacement, updatedAt, snapshot.ConnectionID, snapshot.Provider, snapshot.AuthorizationRevision, snapshot.EncryptedCredentials)
	if err != nil {
		return fmt.Errorf("persist credentials: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAuthorityChanged
	}
	return nil
}

// ValidateAuthorityTickets performs a fresh local rights check. Page-specific
// time-window and freshness checks belong to the page reader's snapshot.
func (s *Store) ValidateAuthorityTickets(ctx context.Context, tickets []AuthorityTicket) error {
	if len(tickets) == 0 {
		return ErrAuthorityChanged
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if s.dialect == DialectPostgres {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := make([]string, 0, len(tickets))
	args := make([]any, 0, len(tickets)*4+1)
	for _, t := range tickets {
		if t.AuthorizationRevision == "" || t.DetailReceipt == "" || len(t.DetailReceipt) > 1024 {
			return ErrAuthorityChanged
		}
		values = append(values, "(?,?,?,?)")
		args = append(args, t.ConnectionID, t.CalendarID, t.AuthorizationRevision, t.DetailReceipt)
	}
	args = append(args, true)
	// Each input row participates separately, including duplicates. A count of
	// matching tuples therefore rejects even a single mismatched proof.
	query := `WITH expected(connection_id,calendar_id,revision,receipt) AS (VALUES ` + strings.Join(values, ",") + `)
SELECT COUNT(*) FROM expected e
JOIN connections c ON c.id=e.connection_id
JOIN calendars cal ON cal.connection_id=c.id AND cal.id=e.calendar_id
JOIN calendar_sync_state st ON st.calendar_id=cal.id
WHERE c.status='connected' AND c.authorization_revision=e.revision AND cal.detail_receipt=e.receipt
AND cal.can_read=? AND cal.enabled=TRUE AND (st.last_error_code IS NULL OR st.last_error_code NOT IN ('auth','permission'))`
	var count int
	if err = tx.QueryRowContext(ctx, s.query(query), args...).Scan(&count); err != nil {
		return err
	}
	if count != len(tickets) {
		return ErrAuthorityChanged
	}
	return tx.Commit()
}

// BeginAuthorization invalidates all prior detail proofs and leases before
// purging their projection. A nil replacement keeps the captured credentials.
func (s *Store) BeginAuthorization(ctx context.Context, id string, replacement []byte, now time.Time) (CredentialSnapshot, error) {
	return s.beginAuthorization(ctx, id, replacement, now, false)
}

func (s *Store) beginAuthorization(ctx context.Context, id string, replacement []byte, now time.Time, legacyOnly bool) (CredentialSnapshot, error) {
	var out CredentialSnapshot
	if replacement != nil && len(replacement) > 16384 {
		return out, ErrAuthorityChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	revision := uuid.NewString()
	q := `UPDATE connections SET authorization_revision=?, status='pending', last_error_code=NULL, updated_at=? WHERE id=?`
	args := []any{revision, now, id}
	if replacement != nil {
		q = `UPDATE connections SET authorization_revision=?, status='pending', last_verified_at=NULL, last_error_code=NULL, updated_at=?, encrypted_credentials=? WHERE id=?`
		args = []any{revision, now, replacement, id}
	}
	if legacyOnly {
		q += ` AND provider='google' AND status='connected' AND authorization_revision='' AND EXISTS (SELECT 1 FROM calendars WHERE connection_id=connections.id AND can_read=? AND detail_receipt='')`
		args = append(args, true)
	}
	result, err := tx.ExecContext(ctx, s.query(q), args...)
	if err != nil {
		return out, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return out, err
	}
	if n != 1 {
		return out, ErrNotFound
	}
	// Connection is locked first; update sync states before touching projection rows.
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET detail_receipt='', can_read=CASE WHEN (SELECT provider FROM connections WHERE id=?)='google' THEN ? ELSE can_read END, google_primary=CASE WHEN (SELECT provider FROM connections WHERE id=?)='google' THEN ? ELSE google_primary END WHERE connection_id=?`), id, false, id, false, id); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1, cursor='', status='pending', lease_owner=NULL, lease_until=NULL, last_success_at=NULL, next_sync_at=?, last_error_code=NULL, updated_at=? WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)`), now, now, id); err != nil {
		return out, err
	}
	if err = s.purgeAuthorizedProjection(ctx, tx, projectionAccountScope, id); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, s.query(`SELECT id, provider, authorization_revision, status, encrypted_credentials, credential_version FROM connections WHERE id=? AND credential_is_bounded`), id).Scan(&out.ConnectionID, &out.Provider, &out.AuthorizationRevision, &out.Status, &out.EncryptedCredentials, &out.CredentialVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrAuthorityChanged
	}
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

// DiscoveredDetailCalendar is one entry in a complete Google calendar list.
type DiscoveredDetailCalendar struct {
	Calendar   Calendar
	AccessRole string
}

// DetailReceiptRole extracts the role that produced the receipt. An unknown or
// malformed receipt never conveys detail access.
func DetailReceiptRole(receipt string) string {
	role, id, ok := strings.Cut(receipt, ":")
	if !ok || len(receipt) > 1024 {
		return ""
	}
	if _, err := uuid.Parse(id); err != nil {
		return ""
	}
	switch role {
	case "owner", "writer", "reader", "writerWithoutPrivateAccess":
		return role
	}
	return ""
}

// CompleteGoogleAuthorization atomically publishes an entire already-staged
// discovery, including an empty list. It never opens denied roles.
func (s *Store) CompleteGoogleAuthorization(ctx context.Context, snapshot CredentialSnapshot, staged []DiscoveredDetailCalendar, now time.Time) error {
	return s.completeGoogleAuthorization(ctx, snapshot, staged, "", "", now)
}

// CompleteGoogleAuthorizationWithIdentity publishes discovery and identity in
// the same fenced transaction. Empty identity leaves the existing value alone.
func (s *Store) CompleteGoogleAuthorizationWithIdentity(ctx context.Context, snapshot CredentialSnapshot, staged []DiscoveredDetailCalendar, fingerprint, displayName string, now time.Time) error {
	return s.completeGoogleAuthorization(ctx, snapshot, staged, fingerprint, displayName, now)
}
func (s *Store) completeGoogleAuthorization(ctx context.Context, snapshot CredentialSnapshot, staged []DiscoveredDetailCalendar, fingerprint, displayName string, now time.Time) error {
	if len(staged) > 256 || snapshot.AuthorizationRevision == "" {
		return ErrAuthorityChanged
	}
	seen := make(map[string]struct{}, len(staged))
	primaryCount := 0
	for _, entry := range staged {
		c := entry.Calendar
		if c.ConnectionID != snapshot.ConnectionID || c.ID == "" || c.ProviderCalendarID == "" {
			return ErrAuthorityChanged
		}
		if _, ok := seen[c.ProviderCalendarID]; ok {
			return ErrAuthorityChanged
		}
		seen[c.ProviderCalendarID] = struct{}{}
		if c.GooglePrimary {
			primaryCount++
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The conditional update acquires the connection lock before calendar rows.
	q := `UPDATE connections SET status='connected', last_verified_at=?, last_error_code=NULL, updated_at=? WHERE id=? AND provider='google' AND authorization_revision=? AND status='pending'`
	args := []any{now, now, snapshot.ConnectionID, snapshot.AuthorizationRevision}
	if fingerprint != "" {
		q = `UPDATE connections SET status='connected', last_verified_at=?, last_error_code=NULL, updated_at=?, account_fingerprint=?, display_name=? WHERE id=? AND provider='google' AND authorization_revision=? AND status='pending'`
		args = []any{now, now, fingerprint, displayName, snapshot.ConnectionID, snapshot.AuthorizationRevision}
	}
	result, err := tx.ExecContext(ctx, s.query(q), args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAuthorityChanged
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET can_read=?, can_write=?, supports_recurrence=?, google_primary=?, detail_receipt='', discovered_at=? WHERE connection_id=?`), false, false, false, false, now.Add(-time.Second), snapshot.ConnectionID); err != nil {
		return err
	}
	for _, entry := range staged {
		c := entry.Calendar
		receipt := ""
		role := entry.AccessRole
		switch role {
		case "owner", "writer", "reader", "writerWithoutPrivateAccess":
			receipt = role + ":" + uuid.NewString()
		}
		readable := receipt != ""
		writable := role == "owner" || role == "writer" || role == "writerWithoutPrivateAccess"
		_, err = tx.ExecContext(ctx, s.query(`INSERT INTO calendars(id,connection_id,provider_calendar_id,name,timezone,can_read,can_write,supports_recurrence,discovered_at,google_primary,detail_receipt) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(connection_id,provider_calendar_id) DO UPDATE SET name=excluded.name,timezone=CASE WHEN excluded.timezone<>'' THEN excluded.timezone ELSE calendars.timezone END,can_read=excluded.can_read,can_write=excluded.can_write,supports_recurrence=excluded.supports_recurrence,discovered_at=excluded.discovered_at,google_primary=excluded.google_primary,detail_receipt=excluded.detail_receipt`), c.ID, c.ConnectionID, c.ProviderCalendarID, c.Name, safeDiscoveryTimezone("google", c.Timezone), readable, writable, readable, now, c.GooglePrimary && primaryCount == 1, receipt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FailAuthorization closes only the still-current verification attempt.
func (s *Store) FailAuthorization(ctx context.Context, snapshot CredentialSnapshot, now time.Time) error {
	result, err := s.db.ExecContext(ctx, s.query(`UPDATE connections SET status='error',last_error_code='provider_verification_failed',updated_at=? WHERE id=? AND authorization_revision=? AND status='pending'`), now, snapshot.ConnectionID, snapshot.AuthorizationRevision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAuthorityChanged
	}
	return nil
}

// CaptureWriterTicket obtains projection authority before provider I/O.
func (s *Store) CaptureWriterTicket(ctx context.Context, calendarID string) (AuthorityTicket, error) {
	var t AuthorityTicket
	var provider, status string
	var readable bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT c.id,cal.id,c.authorization_revision,cal.detail_receipt,st.generation,c.provider,c.status,(cal.can_read AND cal.enabled) FROM calendars cal JOIN connections c ON c.id=cal.connection_id JOIN calendar_sync_state st ON st.calendar_id=cal.id WHERE cal.id=?`), calendarID).Scan(&t.ConnectionID, &t.CalendarID, &t.AuthorizationRevision, &t.DetailReceipt, &t.Generation, &provider, &status, &readable)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if status != "connected" || !readable || (provider == "google" && (t.AuthorizationRevision == "" || DetailReceiptRole(t.DetailReceipt) == "")) {
		return t, ErrAuthorityChanged
	}
	return t, nil
}

func validateWriterTicket(ctx context.Context, s *Store, tx *sql.Tx, t AuthorityTicket) error {
	if t.ConnectionID == "" || t.CalendarID == "" {
		return ErrAuthorityChanged
	}
	if err := lockCalendarConnection(ctx, s, tx, t.CalendarID); err != nil {
		return err
	}
	var revision, receipt, status, provider string
	var readable bool
	err := tx.QueryRowContext(ctx, s.query(`SELECT c.authorization_revision,cal.detail_receipt,c.status,c.provider,(cal.can_read AND cal.enabled) FROM calendars cal JOIN connections c ON c.id=cal.connection_id WHERE cal.id=? AND c.id=?`), t.CalendarID, t.ConnectionID).Scan(&revision, &receipt, &status, &provider, &readable)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuthorityChanged
	}
	if err != nil {
		return err
	}
	if revision != t.AuthorizationRevision || receipt != t.DetailReceipt || status != "connected" || !readable || (provider == "google" && (revision == "" || DetailReceiptRole(receipt) == "")) {
		return ErrAuthorityChanged
	}
	return nil
}

func (s *Store) UpsertCachedEventWithTicket(ctx context.Context, t AuthorityTicket, event calendar.EventV2, now time.Time) error {
	if event.CalendarID != t.CalendarID {
		return ErrAuthorityChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateWriterTicket(ctx, s, tx, t); err != nil {
		return err
	}
	state, err := selectCalendarSyncStateForUpdate(ctx, s, tx, t.CalendarID)
	if err != nil {
		return err
	}
	if state.Generation != t.Generation {
		return ErrAuthorityChanged
	}
	if err = upsertCachedEvent(ctx, s, tx, event, event.ID, state.Generation, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteCachedEventWithTicket(ctx context.Context, t AuthorityTicket, ref calendar.EventRef, now time.Time) error {
	if ref.CalendarID != t.CalendarID || ref.EventID == "" {
		return ErrAuthorityChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateWriterTicket(ctx, s, tx, t); err != nil {
		return err
	}
	state, err := selectCalendarSyncStateForUpdate(ctx, s, tx, t.CalendarID)
	if err != nil {
		return err
	}
	if state.Generation != t.Generation {
		return ErrAuthorityChanged
	}
	if err = tombstoneCachedEvent(ctx, s, tx, ref, state.Generation, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ObserveGoogleRole closes the current source proof before a mismatched or
// absent Events.List role can be applied, including on an empty page.
func (s *Store) ObserveGoogleRole(ctx context.Context, t AuthorityTicket, revision string, digest [32]byte, role string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateWriterTicket(ctx, s, tx, t); err != nil {
		return err
	}
	if revision != t.AuthorizationRevision {
		return ErrAuthorityChanged
	}
	var ciphertext []byte
	err = tx.QueryRowContext(ctx, s.query(`SELECT encrypted_credentials FROM connections WHERE id=? AND credential_is_bounded`), t.ConnectionID).Scan(&ciphertext)
	if err != nil {
		return err
	}
	if sha256.Sum256(ciphertext) != digest {
		return ErrAuthorityChanged
	}
	_, err = selectCalendarSyncStateForUpdate(ctx, s, tx, t.CalendarID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if role != "" && role == DetailReceiptRole(t.DetailReceipt) {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET detail_receipt='',can_read=?,can_write=? WHERE id=?`), false, false, t.CalendarID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,cursor='',status='parked',lease_owner=NULL,lease_until=NULL,last_success_at=NULL,last_error_code='authority_changed',updated_at=? WHERE calendar_id=?`), now, t.CalendarID); err != nil {
		return err
	}
	if err = s.purgeAuthorizedProjection(ctx, tx, projectionCalendarScope, t.CalendarID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return errors.Join(ErrAuthorityChanged, ErrAuthorityClosed)
}

// BeginOneLegacyGoogleAuthorization claims at most one eligible account from
// before the authority migration. Ordinary discovery can add metadata without
// a receipt, which never makes an authorized account a bootstrap candidate.
func (s *Store) BeginOneLegacyGoogleAuthorization(ctx context.Context, now time.Time) (*CredentialSnapshot, error) {
	var id string
	var bounded bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT c.id, credential_is_bounded FROM connections c WHERE c.provider='google' AND c.status='connected' AND c.authorization_revision='' AND EXISTS (SELECT 1 FROM calendars cal WHERE cal.connection_id=c.id AND cal.can_read=? AND cal.detail_receipt='') ORDER BY c.id LIMIT 1`), true).Scan(&id, &bounded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bounded {
		return nil, s.failUnsuitableLegacyGoogle(ctx, id, now)
	}
	snapshot, err := s.beginAuthorization(ctx, id, nil, now, true)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// failUnsuitableLegacyGoogle closes only the same still-unsuitable eligible row.
func (s *Store) failUnsuitableLegacyGoogle(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET authorization_revision=?,status='error',last_verified_at=NULL,last_error_code='auth',updated_at=? WHERE id=? AND provider='google' AND status='connected' AND authorization_revision='' AND NOT (credential_is_bounded) AND EXISTS (SELECT 1 FROM calendars WHERE connection_id=connections.id AND can_read=? AND detail_receipt='')`), uuid.NewString(), now, id, true)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET detail_receipt='',can_read=?,can_write=?,google_primary=? WHERE connection_id=?`), false, false, false, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,cursor='',status='parked',lease_owner=NULL,lease_until=NULL,last_success_at=NULL,last_error_code='auth',updated_at=? WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)`), now, id); err != nil {
		return err
	}
	if err = s.purgeAuthorizedProjection(ctx, tx, projectionAccountScope, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CloseGoogleAccountIfCurrent fences all sources only for the exact token
// snapshot that produced a confirmed account-wide authorization denial.
func (s *Store) CloseGoogleAccountIfCurrent(ctx context.Context, t AuthorityTicket, revision string, digest [32]byte, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if t.ConnectionID == "" || revision == "" {
		return ErrAuthorityChanged
	}
	if s.dialect == DialectSQLite {
		if _, err = tx.ExecContext(ctx, s.query(`UPDATE connections SET updated_at=updated_at WHERE id=?`), t.ConnectionID); err != nil {
			return err
		}
	}
	q := `SELECT encrypted_credentials FROM connections WHERE id=? AND provider='google' AND status='connected' AND authorization_revision=? AND credential_is_bounded`
	if s.dialect == DialectPostgres {
		q += " FOR UPDATE"
	}
	var cipher []byte
	err = tx.QueryRowContext(ctx, s.query(q), t.ConnectionID, revision).Scan(&cipher)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuthorityChanged
	}
	if err != nil {
		return err
	}
	if sha256.Sum256(cipher) != digest {
		return ErrAuthorityChanged
	}
	result, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET authorization_revision=?,status='error',last_error_code='auth',updated_at=? WHERE id=? AND authorization_revision=?`), uuid.NewString(), now, t.ConnectionID, revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAuthorityChanged
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET detail_receipt='',can_read=?,can_write=?,google_primary=? WHERE connection_id=?`), false, false, false, t.ConnectionID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,cursor='',status='parked',lease_owner=NULL,lease_until=NULL,last_success_at=NULL,last_error_code='auth',updated_at=? WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)`), now, t.ConnectionID); err != nil {
		return err
	}
	if err = s.purgeAuthorizedProjection(ctx, tx, projectionAccountScope, t.ConnectionID); err != nil {
		return err
	}
	return tx.Commit()
}

// CaptureAnyGoogleTicket finds one currently open source for an account-wide
// denial observed during provider-wide discovery.
func (s *Store) CaptureAnyGoogleTicket(ctx context.Context, id string) (AuthorityTicket, error) {
	var calendarID string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT cal.id FROM calendars cal JOIN connections c ON c.id=cal.connection_id WHERE c.id=? AND c.provider='google' AND c.status='connected' AND c.authorization_revision<>'' AND cal.can_read=? AND cal.detail_receipt<>'' ORDER BY cal.id LIMIT 1`), id, true).Scan(&calendarID)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthorityTicket{}, ErrNotFound
	}
	if err != nil {
		return AuthorityTicket{}, err
	}
	return s.CaptureGoogleSourceTicketByID(ctx, id, calendarID)
}

// CaptureGoogleSourceTicket resolves provider-local IDs, including Google's
// primary alias, only within the already-selected account.
func (s *Store) CaptureGoogleSourceTicket(ctx context.Context, connectionID, providerCalendarID string) (AuthorityTicket, error) {
	var t AuthorityTicket
	var status string
	var readable bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT c.id,cal.id,c.authorization_revision,cal.detail_receipt,COALESCE(st.generation,-1),c.status,cal.can_read FROM calendars cal JOIN connections c ON c.id=cal.connection_id LEFT JOIN calendar_sync_state st ON st.calendar_id=cal.id WHERE cal.connection_id=? AND (cal.provider_calendar_id=? OR (?='primary' AND cal.google_primary=?)) ORDER BY CASE WHEN cal.provider_calendar_id=? THEN 0 ELSE 1 END LIMIT 1`), connectionID, providerCalendarID, providerCalendarID, true, providerCalendarID).Scan(&t.ConnectionID, &t.CalendarID, &t.AuthorizationRevision, &t.DetailReceipt, &t.Generation, &status, &readable)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if status != "connected" || !readable || t.AuthorizationRevision == "" || DetailReceiptRole(t.DetailReceipt) == "" {
		return t, ErrAuthorityChanged
	}
	return t, nil
}

func (s *Store) CaptureGoogleSourceTicketByID(ctx context.Context, connectionID, id string) (AuthorityTicket, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT provider_calendar_id FROM calendars WHERE id=? AND connection_id=?`), id, connectionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthorityTicket{}, ErrNotFound
	}
	if err != nil {
		return AuthorityTicket{}, err
	}
	return s.CaptureGoogleSourceTicket(ctx, connectionID, raw)
}

// CaptureGoogleAccountTickets captures a bounded snapshot before CalendarList I/O.
func (s *Store) CaptureGoogleAccountTickets(ctx context.Context, id string) (map[string]AuthorityTicket, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT cal.provider_calendar_id,c.id,cal.id,c.authorization_revision,cal.detail_receipt,COALESCE(st.generation,-1) FROM calendars cal JOIN connections c ON c.id=cal.connection_id LEFT JOIN calendar_sync_state st ON st.calendar_id=cal.id WHERE c.id=? AND c.provider='google' AND c.status='connected' AND c.authorization_revision<>'' AND cal.can_read=? AND cal.detail_receipt<>'' LIMIT 257`), id, true)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]AuthorityTicket)
	count := 0
	for rows.Next() {
		count++
		var raw string
		var t AuthorityTicket
		if err := rows.Scan(&raw, &t.ConnectionID, &t.CalendarID, &t.AuthorizationRevision, &t.DetailReceipt, &t.Generation); err != nil {
			return nil, err
		}
		if count > 256 {
			return nil, fmt.Errorf("oversized Google authority snapshot")
		}
		if DetailReceiptRole(t.DetailReceipt) != "" {
			result[raw] = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

const (
	projectionAccountScope  = `calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)`
	projectionCalendarScope = `calendar_id=?`
)

// purgeAuthorizedProjection retires current verified snapshots without
// modifying historical revisions still referenced by sync checkpoints or pins.
// The caller already fenced the sync states and holds the account write lock.
func (s *Store) purgeAuthorizedProjection(ctx context.Context, tx *sql.Tx, scope string, id string) error {
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_guards SET epoch=epoch+1 WHERE `+scope), id); err != nil {
		return fmt.Errorf("invalidate source projection guard: %w", err)
	}
	for _, table := range []string{"cached_events", "calendar_sync_objects", "calendar_sync_quarantine", "projection_page_attempts", "projection_snapshot_staging"} {
		if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM `+table+` WHERE `+scope), id); err != nil {
			return fmt.Errorf("purge authorized projection: %w", err)
		}
	}
	return nil
}
