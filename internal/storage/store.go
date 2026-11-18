package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
)

var (
	ErrNotFound                    = errors.New("storage record not found")
	ErrSchemaMismatch              = errors.New("storage schema version mismatch")
	ErrOAuthNotConsumable          = errors.New("oauth attempt is expired, consumed, or missing")
	ErrConnectionInUse             = errors.New("connection is referenced by a sync rule")
	ErrRuleCycle                   = errors.New("sync rule would create a cycle")
	ErrJobLeaseLost                = errors.New("sync job lease is no longer owned by this worker attempt")
	ErrCalendarSyncActive          = errors.New("calendar sync has an active lease")
	ErrCalendarSyncIneligible      = errors.New("calendar sync calendar is not readable on a connected connection")
	ErrInvalidSyncCode             = errors.New("calendar sync error code is invalid")
	ErrEventSyncRepairETagMismatch = errors.New("event sync repair authorization does not match current object version")
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrations embed.FS

type Store struct {
	db                 *sql.DB
	dialect            Dialect
	artifactCipher     *credentials.Cipher
	sqliteTimeUnit     int64
	sqliteTimeLocation *time.Location
}

// SetArtifactCipher enables encrypted persistence of provider diagnostics.
// It is configured by the runtime after validating CALENDAR_ENCRYPTION_KEY.
func (s *Store) SetArtifactCipher(cipher *credentials.Cipher) { s.artifactCipher = cipher }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	dialect, driver, dsn, err := parseDatabaseURL(databaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if dialect == DialectSQLite {
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	var unit int64
	var location *time.Location
	if dialect == DialectSQLite {
		// Match the driver's interpretation of the exact SQLite DSN.
		_, options, _ := strings.Cut(dsn, "?")
		params, _ := url.ParseQuery(options)
		switch params.Get("_time_integer_format") {
		case "unix":
			unit = 1
		case "unix_milli":
			unit = 1000
		case "unix_micro":
			unit = 1000000
		case "unix_nano":
			unit = 1000000000
		}
		if zone := params.Get("_timezone"); zone != "" {
			// Ping has already confirmed that modernc accepts this location.
			location, _ = time.LoadLocation(zone)
		}
	}
	return &Store{db: db, dialect: dialect, sqliteTimeUnit: unit, sqliteTimeLocation: location}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func parseDatabaseURL(raw string) (Dialect, string, string, error) {
	if raw == "" {
		return "", "", "", errors.New("DATABASE_URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		return DialectPostgres, "pgx", raw, nil
	case "sqlite":
		dsn := strings.TrimPrefix(raw, "sqlite://")
		if dsn == "" {
			return "", "", "", errors.New("sqlite DATABASE_URL requires a path")
		}
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn += separator + "_pragma=foreign_keys%28ON%29&_pragma=busy_timeout%285000%29&_pragma=journal_mode%28WAL%29"
		return DialectSQLite, "sqlite", dsn, nil
	default:
		return "", "", "", fmt.Errorf("unsupported DATABASE_URL scheme %q", u.Scheme)
	}
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == DialectPostgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(192836421)"); err != nil {
			return fmt.Errorf("lock migrations: %w", err)
		}
	}
	migrationTable := "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL)"
	if s.dialect == DialectPostgres {
		migrationTable = "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)"
	}
	if _, err := tx.ExecContext(ctx, migrationTable); err != nil {
		return fmt.Errorf("prepare migration table: %w", err)
	}
	var version int
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w: database=%d binary=%d", ErrSchemaMismatch, version, SchemaVersion)
	}
	for next := version + 1; next <= SchemaVersion; next++ {
		path := fmt.Sprintf("migrations/%s/%03d_%s.sql", s.dialect, next, migrationName(next))
		script, err := migrations.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read schema version %d: %w", next, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("apply schema version %d: %w", next, err)
		}
		if _, err := tx.ExecContext(ctx, s.query("INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)"), next, time.Now().UTC()); err != nil {
			return fmt.Errorf("record schema version %d: %w", next, err)
		}
	}
	if err := validateMigrationSchema(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// The abandoned evidence experiment used the same version number without this
// column. A version number alone must not make that schema appear compatible.
func validateMigrationSchema(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) error {
	for _, check := range []struct{ query, message string }{
		{`SELECT stop_requested FROM sync_jobs WHERE 1=0`, "sync stop-request schema is missing"},
		{`SELECT authorization_revision FROM connections WHERE 1=0`, "connection authority schema is missing"},
		{`SELECT detail_receipt FROM calendars WHERE 1=0`, "calendar authority schema is missing"},
	} {
		rows, err := db.QueryContext(ctx, check.query)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrSchemaMismatch, check.message)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func migrationName(version int) string {
	switch version {
	case 1:
		return "initial"
	case 2:
		return "event_read_model"
	case 3:
		return "event_sync_quarantine"
	case 4:
		return "event_sync_raw_artifacts"
	case 5:
		return "event_sync_repair_authorization"
	case 6:
		return "event_sync_provider_corrections"
	case 7:
		return "google_primary_calendar"
	case 8:
		return "calendar_enabled"
	case 9:
		return "calendar_display_settings"
	case 10:
		return "sync_run_preview"
	case 11:
		return "projection_mutations"
	case 12:
		return "removal_staging"
	case 13:
		return "rule_config_version"
	case 14:
		return "projection_page_staging"
	case 15:
		return "target_operation_intents"
	case 16:
		return "projection_revision_provenance"
	case 17:
		return "snapshot_pins"
	case 18:
		return "projection_mutation_many"
	case 19:
		return "projection_checkpoints"
	case 20:
		return "operator_repair_metadata"
	case 21:
		return "cache_mappings"
	case 22:
		return "target_operation_receipts"
	case 23:
		return "target_guard_links"
	case 24:
		return "exclusive_dispatch_fence"
	case 25:
		return "job_lease_deadline"
	case 26:
		return "mapping_version"
	case 27:
		return "rule_sync_mode"
	case 28:
		return "sync_stop_request"
	case 29:
		return "cached_detail_authority"
	default:
		return ""
	}
}

func (s *Store) CheckSchema(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return fmt.Errorf("%w: %v", ErrSchemaMismatch, err)
	}
	if version != SchemaVersion {
		return fmt.Errorf("%w: database=%d binary=%d", ErrSchemaMismatch, version, SchemaVersion)
	}
	return validateMigrationSchema(ctx, s.db)
}

func (s *Store) query(q string) string {
	guard := "(typeof(encrypted_credentials)='blob' AND length(encrypted_credentials)<=16384)"
	if s.dialect == DialectPostgres {
		guard = "(octet_length(encrypted_credentials)<=16384)"
	}
	q = strings.ReplaceAll(q, "credential_is_bounded", guard)
	if s.dialect != DialectPostgres {
		return q
	}
	var b strings.Builder
	n := 1
	for _, r := range q {
		if r == '?' {
			fmt.Fprintf(&b, "$%d", n)
			n++
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Store) CreateConnection(ctx context.Context, c Connection) error {
	c.AuthorizationRevision = uuid.NewString()
	if c.AccountFingerprint == "" {
		c.AccountFingerprint = "pending:" + c.ID
	}
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO connections
		(id, provider, account_fingerprint, display_name, status, encrypted_credentials, credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at, authorization_revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		c.ID, c.Provider, c.AccountFingerprint, c.DisplayName, c.Status, c.EncryptedCredentials, c.CredentialVersion, c.ScopesJSON,
		c.LastVerifiedAt, nullString(c.LastErrorCode), c.CreatedAt, c.UpdatedAt, c.AuthorizationRevision)
	if err != nil {
		return fmt.Errorf("create connection: %w", err)
	}
	return nil
}

func (s *Store) ConnectionByProvider(ctx context.Context, provider string) (Connection, error) {
	row := s.db.QueryRowContext(ctx, s.query(s.timeProjection(`SELECT id, provider, account_fingerprint, display_name, status, CASE WHEN credential_is_bounded THEN encrypted_credentials ELSE NULL END,
		credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at, authorization_revision FROM connections WHERE provider = ?`, "last_verified_at", "created_at", "updated_at")), provider)
	return s.scanConnection(row)
}

func (s *Store) ConnectionByID(ctx context.Context, id string) (Connection, error) {
	row := s.db.QueryRowContext(ctx, s.query(s.timeProjection(`SELECT id, provider, account_fingerprint, display_name, status, CASE WHEN credential_is_bounded THEN encrypted_credentials ELSE NULL END,
		credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at, authorization_revision FROM connections WHERE id = ?`, "last_verified_at", "created_at", "updated_at")), id)
	return s.scanConnection(row)
}

func (s *Store) UpdateConnectionCredentials(ctx context.Context, id string, encrypted []byte, version int, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE connections SET encrypted_credentials=?, credential_version=?, status=?, last_verified_at=NULL, last_error_code=NULL, updated_at=? WHERE id=?`), encrypted, version, "pending", updatedAt, id)
	if err != nil {
		return fmt.Errorf("update connection credentials: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateConnectionVerification(ctx context.Context, id, status, errorCode string, verifiedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connection verification update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET status=?,last_verified_at=?,last_error_code=?,updated_at=?,authorization_revision=CASE WHEN provider='google' AND ?<>'connected' THEN ? ELSE authorization_revision END WHERE id=?`), status, verifiedAt, nullString(errorCode), verifiedAt, status, uuid.NewString(), id)
	if err != nil {
		return fmt.Errorf("update connection verification: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	if status != "connected" {
		var provider string
		if err := tx.QueryRowContext(ctx, s.query(`SELECT provider FROM connections WHERE id=?`), id).Scan(&provider); err != nil {
			return err
		}
		if provider == "google" {
			if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendars SET detail_receipt='',can_read=?,can_write=?,google_primary=? WHERE connection_id=?`), false, false, false, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,cursor='',status='parked',lease_owner=NULL,lease_until=NULL,last_success_at=NULL,updated_at=? WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)`), verifiedAt, id); err != nil {
				return err
			}
			if err := s.purgeAuthorizedProjection(ctx, tx, projectionAccountScope, id); err != nil {
				return err
			}
		}
	}
	if status == "connected" {
		if _, err := tx.ExecContext(ctx, s.query(s.eventSyncTimeComparisons(`UPDATE calendar_sync_state
			SET status=?, next_sync_at=?, last_error_code=NULL, updated_at=?
			WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)
				AND status=? AND (lease_until IS NULL OR lease_until<=?)`)), "pending", verifiedAt, verifiedAt, id, "parked", verifiedAt); err != nil {
			return fmt.Errorf("reactivate parked calendar sync states: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit connection verification update: %w", err)
	}
	return nil
}

func (s *Store) UpdateConnectionIdentity(ctx context.Context, id, fingerprint, displayName string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE connections SET account_fingerprint=?, display_name=?, updated_at=? WHERE id=?`), fingerprint, displayName, updatedAt, id)
	if err != nil {
		return fmt.Errorf("update connection identity: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize delete with authority transitions and all projection writers.
	if s.dialect == DialectSQLite {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET updated_at=updated_at WHERE id=?`), id); err != nil {
			return err
		}
	}
	lockQuery := `SELECT id FROM connections WHERE id=?`
	if s.dialect == DialectPostgres {
		lockQuery += " FOR UPDATE"
	}
	var locked string
	err = tx.QueryRowContext(ctx, s.query(lockQuery), id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var references int
	err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM sync_rules r
		JOIN calendars source ON source.id=r.source_calendar_id
		JOIN calendars target ON target.id=r.target_calendar_id
		WHERE source.connection_id=? OR target.connection_id=?`), id, id).Scan(&references)
	if err != nil {
		return fmt.Errorf("check connection references: %w", err)
	}
	if references > 0 {
		return ErrConnectionInUse
	}
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,lease_owner=NULL,lease_until=NULL WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)`), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM calendars WHERE connection_id=?`), id); err != nil {
		return fmt.Errorf("delete connection calendars: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.query(`DELETE FROM connections WHERE id=?`), id)
	if err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, s.query(s.timeProjection(`SELECT id, provider, account_fingerprint, display_name, status, CASE WHEN credential_is_bounded THEN encrypted_credentials ELSE NULL END,
		credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at, authorization_revision FROM connections ORDER BY provider`, "last_verified_at", "created_at", "updated_at")))
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		c, err := s.scanConnection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func (s *Store) scanConnection(row rowScanner) (Connection, error) {
	var c Connection
	verified, created, updated := s.scanTime(), s.scanTime(), s.scanTime()
	var errorCode sql.NullString
	if err := row.Scan(&c.ID, &c.Provider, &c.AccountFingerprint, &c.DisplayName, &c.Status, &c.EncryptedCredentials,
		&c.CredentialVersion, &c.ScopesJSON, &verified, &errorCode, &created, &updated, &c.AuthorizationRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c, ErrNotFound
		}
		return c, fmt.Errorf("scan connection: %w", err)
	}
	if verified.Valid {
		c.LastVerifiedAt = &verified.Time
	}
	c.CreatedAt, c.UpdatedAt = created.Time, updated.Time
	c.LastErrorCode = errorCode.String
	return c, nil
}

// safeDiscoveryTimezone accepts only explicit, loadable IANA zone names.
// Invalid or absent metadata never overwrites a previously known zone.
func safeDiscoveryTimezone(zone string) string {
	if zone == "" || zone == "Local" {
		return ""
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return ""
	}
	return zone
}

func (s *Store) UpsertCalendar(ctx context.Context, c Calendar) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize source permission changes with reconnect and sync writers.
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE connections SET updated_at=updated_at WHERE id=?`), c.ConnectionID); err != nil {
		return err
	}
	var provider string
	if err = tx.QueryRowContext(ctx, s.query(`SELECT provider FROM connections WHERE id=?`), c.ConnectionID).Scan(&provider); err != nil {
		return err
	}
	var readable bool
	var existingID string
	err = tx.QueryRowContext(ctx, s.query(`SELECT id,can_read FROM calendars WHERE connection_id=? AND provider_calendar_id=?`), c.ConnectionID, c.ProviderCalendarID).Scan(&existingID, &readable)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	transition := err == nil && provider != "google" && readable != c.CanRead
	if err = upsertCalendarWithAuthority(ctx, s, tx, c); err != nil {
		return err
	}
	if transition {
		if err = s.purgeNonGoogleSource(ctx, tx, existingID, c.DiscoveredAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upsertCalendarWithAuthority(ctx context.Context, s *Store, db execer, c Calendar) error {
	c.Timezone = safeDiscoveryTimezone(c.Timezone)
	q := `INSERT INTO calendars(id, connection_id, provider_calendar_id, name, timezone, can_read, can_write, supports_recurrence, discovered_at, google_primary, detail_receipt)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CASE WHEN (SELECT provider FROM connections WHERE id=?)<>'google' THEN ? ELSE '' END)
 ON CONFLICT(connection_id, provider_calendar_id) DO UPDATE SET
 name=excluded.name, timezone=CASE WHEN excluded.timezone<>'' THEN excluded.timezone ELSE calendars.timezone END, can_read=excluded.can_read, can_write=excluded.can_write,
 supports_recurrence=excluded.supports_recurrence, discovered_at=excluded.discovered_at, google_primary=excluded.google_primary,
 detail_receipt=CASE WHEN (SELECT provider FROM connections WHERE id=excluded.connection_id)<>'google' AND (calendars.can_read<>excluded.can_read OR calendars.detail_receipt='') THEN excluded.detail_receipt ELSE calendars.detail_receipt END`
	_, err := db.ExecContext(ctx, s.query(q), c.ID, c.ConnectionID, c.ProviderCalendarID, c.Name, c.Timezone, c.CanRead, c.CanWrite, c.SupportsRecurrence, c.DiscoveredAt, c.GooglePrimary, c.ConnectionID, uuid.NewString())
	if err != nil {
		return fmt.Errorf("upsert calendar: %w", err)
	}
	return nil
}

// RegisterDiscoveredCalendar only inserts a new provider-returned calendar.
// Existing permissions, primary metadata, and the user's selection are untouched.
func (s *Store) RegisterDiscoveredCalendar(ctx context.Context, route string, item calendar.Calendar) error {
	provider, connectionID, ok := strings.Cut(route, "@")
	if !ok || provider == "" || connectionID == "" || item.ID == "" {
		return nil
	}
	return s.registerDiscoveredCalendar(ctx, s.db, provider, connectionID, item, true)
}

// RegisterDiscoveredCalendarWithRevision rejects a stale live-discovery result
// before it can add a calendar to an account that has since been reconnected.
func (s *Store) RegisterDiscoveredCalendarWithRevision(ctx context.Context, route string, item calendar.Calendar, revision string) error {
	provider, connectionID, ok := strings.Cut(route, "@")
	if !ok || provider == "" || connectionID == "" || item.ID == "" {
		return ErrAuthorityChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.dialect == DialectSQLite {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET updated_at=updated_at WHERE id=?`), connectionID); err != nil {
			return err
		}
	}
	q := `SELECT authorization_revision,status,
		EXISTS(SELECT 1 FROM calendars WHERE connection_id=connections.id AND provider_calendar_id=?),
		CASE WHEN provider='google' AND credential_is_bounded THEN encrypted_credentials END
		FROM connections WHERE id=? AND provider=?`
	if s.dialect == DialectPostgres {
		q += " FOR NO KEY UPDATE"
	}
	var current, status string
	var encrypted []byte
	var known bool
	err = tx.QueryRowContext(ctx, s.query(q), item.ID, connectionID, provider).Scan(&current, &status, &known, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuthorityChanged
	}
	if err != nil {
		return err
	}
	if current != revision || status != "connected" {
		return ErrAuthorityChanged
	}
	if known {
		return tx.Commit()
	}
	if provider == "google" && (len(encrypted) == 0 || sha256.Sum256(encrypted) != item.GoogleDiscoveryCredentialDigest) {
		return ErrAuthorityChanged
	}
	// A normal Google list result carries routing metadata. Detailed
	// permissions are opened by completing staged Google authorization.
	if err := s.registerDiscoveredCalendar(ctx, tx, provider, connectionID, item, provider != "google"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) registerDiscoveredCalendar(ctx context.Context, db execer, provider, connectionID string, item calendar.Calendar, readable bool) error {
	receipt := ""
	if provider != "google" {
		receipt = uuid.NewString()
	}
	q := `INSERT INTO calendars(id, connection_id, provider_calendar_id, name, timezone, can_read, can_write, supports_recurrence, discovered_at, google_primary, detail_receipt)
		SELECT ?, id, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM connections WHERE id=? AND provider=? AND status='connected'
		ON CONFLICT(connection_id, provider_calendar_id) DO NOTHING`
	_, err := db.ExecContext(ctx, s.query(q), calendar.AccountCalendarID(provider, connectionID, item.ID), item.ID, item.Name, safeDiscoveryTimezone(item.Timezone), readable, readable && !item.ReadOnly, false, time.Now().UTC(), false, receipt, connectionID, provider)
	if err != nil {
		return fmt.Errorf("register discovered calendar: %w", err)
	}
	return nil
}

// SetCalendarEnabled persists the user's choice and invalidates any in-flight
// projection lease. Existing cached rows are retained for later reconciliation.
func (s *Store) SetCalendarEnabled(ctx context.Context, id string, enabled bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCalendarConnection(ctx, s, tx, id); err != nil {
		return err
	}
	// Preserve the exact granted role but replace its proof so a read that
	// spans disable and re-enable cannot reuse an earlier selection.
	var currentEnabled bool
	var provider, receipt string
	err = tx.QueryRowContext(ctx, s.query(`SELECT cal.enabled,conn.provider,cal.detail_receipt FROM calendars cal
		JOIN connections conn ON conn.id=cal.connection_id WHERE cal.id=?`), id).Scan(&currentEnabled, &provider, &receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentEnabled == enabled {
		return tx.Commit()
	}
	newReceipt := renewedSelectionReceipt(provider, receipt)
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendars SET enabled=?, detail_receipt=? WHERE id=?`), enabled, newReceipt, id); err != nil {
		return fmt.Errorf("update calendar selection: %w", err)
	}
	_, err = tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1, cursor='',
		status=CASE WHEN ? OR status<>'parked' THEN 'pending' ELSE status END,
		next_sync_at=CASE WHEN ? OR status<>'parked' THEN ? ELSE next_sync_at END,
		last_error_code=CASE WHEN ? OR status<>'parked' THEN NULL ELSE last_error_code END,
		last_success_at=NULL, lease_owner=NULL, lease_until=NULL, updated_at=? WHERE calendar_id=?`), enabled, enabled, now, enabled, now, id)
	if err != nil {
		return fmt.Errorf("invalidate calendar sync: %w", err)
	}
	return tx.Commit()
}

func renewedSelectionReceipt(provider, receipt string) string {
	if provider != "google" {
		return uuid.NewString()
	}
	if role := DetailReceiptRole(receipt); role != "" {
		return role + ":" + uuid.NewString()
	}
	return ""
}

func (s *Store) CalendarEnabled(ctx context.Context, id string) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT enabled FROM calendars WHERE id=?`), id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled, err
}

func (s *Store) ClearGooglePrimaryCalendars(ctx context.Context, connectionID string) error {
	_, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET google_primary=? WHERE connection_id=?`), false, connectionID)
	return err
}

func (s *Store) SetGooglePrimaryCalendar(ctx context.Context, connectionID, providerCalendarID string) error {
	_, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET google_primary=(provider_calendar_id=? AND ? <> '') WHERE connection_id=?`), providerCalendarID, providerCalendarID, connectionID)
	return err
}

func (s *Store) ListCalendars(ctx context.Context, connectionID string) ([]Calendar, error) {
	rows, err := s.db.QueryContext(ctx, s.query(s.timeProjection(`SELECT id, connection_id, provider_calendar_id, name, timezone,
		can_read, can_write, supports_recurrence, discovered_at, google_primary, enabled, detail_receipt FROM calendars WHERE connection_id = ? ORDER BY name`, "discovered_at")), connectionID)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	defer rows.Close()
	var result []Calendar
	for rows.Next() {
		var c Calendar
		discovered := s.scanTime()
		if err := rows.Scan(&c.ID, &c.ConnectionID, &c.ProviderCalendarID, &c.Name, &c.Timezone, &c.CanRead, &c.CanWrite, &c.SupportsRecurrence, &discovered, &c.GooglePrimary, &c.Enabled, &c.DetailReceipt); err != nil {
			return nil, err
		}
		c.DiscoveredAt = discovered.Time
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) ListAllCalendars(ctx context.Context) ([]Calendar, error) {
	rows, err := s.db.QueryContext(ctx, s.timeProjection(`SELECT id, connection_id, provider_calendar_id, name, timezone,
		can_read, can_write, supports_recurrence, discovered_at, google_primary, enabled, detail_receipt FROM calendars ORDER BY name`, "discovered_at"))
	if err != nil {
		return nil, fmt.Errorf("list all calendars: %w", err)
	}
	defer rows.Close()
	var result []Calendar
	for rows.Next() {
		var c Calendar
		discovered := s.scanTime()
		if err := rows.Scan(&c.ID, &c.ConnectionID, &c.ProviderCalendarID, &c.Name, &c.Timezone, &c.CanRead, &c.CanWrite, &c.SupportsRecurrence, &discovered, &c.GooglePrimary, &c.Enabled, &c.DetailReceipt); err != nil {
			return nil, err
		}
		c.DiscoveredAt = discovered.Time
		result = append(result, c)
	}
	return result, rows.Err()
}

// ResolveCalendarReference accepts a canonical stored calendar ID or a legacy
// provider:calendar ID. Legacy references are accepted only when they identify
// exactly one connected account.
func (s *Store) ResolveCalendarReference(ctx context.Context, reference string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, s.query("SELECT id FROM calendars WHERE id=?"), reference).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	provider, providerCalendarID, ok := strings.Cut(reference, ":")
	if !ok || provider == "" || providerCalendarID == "" || strings.Contains(provider, "@") {
		return "", ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT c.id FROM calendars c JOIN connections a ON a.id=c.connection_id
		WHERE a.provider=? AND c.provider_calendar_id=? ORDER BY c.id`), provider, providerCalendarID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", ErrNotFound
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("legacy calendar reference %q is ambiguous across %d connected accounts", reference, len(matches))
	}
	return matches[0], nil
}

func normalizedSyncMode(mode string) string {
	if mode == "" {
		return "direct"
	}
	return mode
}

func ValidateRule(r Rule) error {
	if r.SyncMode != "" && r.SyncMode != "direct" && r.SyncMode != "cached_instances" {
		return errors.New("invalid sync mode")
	}
	if r.ID == "" || r.SourceCalendarID == "" || r.TargetCalendarID == "" {
		return errors.New("rule id, source calendar, and target calendar are required")
	}
	if r.SourceCalendarID == r.TargetCalendarID {
		return errors.New("source and target calendars must differ")
	}
	if r.IntervalSeconds != 600 && r.IntervalSeconds != 1800 && r.IntervalSeconds != 3600 {
		return errors.New("interval_seconds must be 600, 1800, or 3600")
	}
	if r.LookbackDays < 0 || r.LookbackDays > 365 {
		return errors.New("lookback_days must be between 0 and 365")
	}
	if r.LookaheadDays < 1 || r.LookaheadDays > 365 {
		return errors.New("lookahead_days must be between 1 and 365")
	}
	if r.CopyAttendees {
		return errors.New("copy_attendees is not supported")
	}
	if r.RecurrenceMode != "preserve" {
		return errors.New("recurrence_mode must be preserve")
	}
	if r.NotificationPolicy != "none" {
		return errors.New("notification_policy must be none")
	}
	return nil
}

func (s *Store) CreateRule(ctx context.Context, r Rule) error {
	if r.SyncMode == "cached_instances" && r.State != "paused" {
		return ErrCacheExecutionIneligible
	}
	if err := ValidateRule(r); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create rule: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockRuleGraph(ctx, tx); err != nil {
		return err
	}
	if err := s.validateRuleRoute(ctx, tx, r); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO sync_rules(id, source_calendar_id, target_calendar_id, state,
		interval_seconds, lookback_days, lookahead_days, recurrence_mode, copy_attendees, notification_policy,
		next_run_at, consecutive_failures, created_at, updated_at, sync_mode) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.SourceCalendarID, r.TargetCalendarID, r.State, r.IntervalSeconds, r.LookbackDays, r.LookaheadDays,
		r.RecurrenceMode, r.CopyAttendees, r.NotificationPolicy, r.NextRunAt, r.ConsecutiveFailures, r.CreatedAt, r.UpdatedAt, normalizedSyncMode(r.SyncMode))
	if err != nil {
		return fmt.Errorf("create rule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create rule: %w", err)
	}
	return nil
}

func (s *Store) lockRuleGraph(ctx context.Context, tx *sql.Tx) error {
	if s.dialect == DialectPostgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(192836422)"); err != nil {
			return fmt.Errorf("lock sync rule graph: %w", err)
		}
	}
	return nil
}

func (s *Store) validateRuleRoute(ctx context.Context, tx *sql.Tx, candidate Rule) error {
	var sourceReadable, targetWritable bool
	if err := tx.QueryRowContext(ctx, s.query("SELECT (can_read AND enabled) FROM calendars WHERE id=?"), candidate.SourceCalendarID).Scan(&sourceReadable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("source calendar is not connected")
		}
		return fmt.Errorf("load source calendar: %w", err)
	}
	if !sourceReadable {
		return errors.New("source calendar is not readable")
	}
	if err := tx.QueryRowContext(ctx, s.query("SELECT (can_write AND enabled) FROM calendars WHERE id=?"), candidate.TargetCalendarID).Scan(&targetWritable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("target calendar is not connected")
		}
		return fmt.Errorf("load target calendar: %w", err)
	}
	if !targetWritable {
		return errors.New("target calendar is not writable")
	}

	rows, err := tx.QueryContext(ctx, "SELECT source_calendar_id, target_calendar_id FROM sync_rules WHERE state<>'archived'")
	if err != nil {
		return fmt.Errorf("load sync rule graph: %w", err)
	}
	defer rows.Close()
	graph := map[string][]string{}
	for rows.Next() {
		var source, target string
		if err := rows.Scan(&source, &target); err != nil {
			return err
		}
		graph[source] = append(graph[source], target)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	graph[candidate.SourceCalendarID] = append(graph[candidate.SourceCalendarID], candidate.TargetCalendarID)
	seen := map[string]bool{}
	var reachesSource func(string) bool
	reachesSource = func(node string) bool {
		if node == candidate.SourceCalendarID {
			return true
		}
		if seen[node] {
			return false
		}
		seen[node] = true
		for _, next := range graph[node] {
			if reachesSource(next) {
				return true
			}
		}
		return false
	}
	if reachesSource(candidate.TargetCalendarID) {
		return ErrRuleCycle
	}
	return nil
}

// ImportLegacy atomically creates a paused rule and its legacy mappings. It
// never calls a calendar provider and refuses to attach mappings to another
// rule or to calendars that have not already been discovered.
func (s *Store) ImportLegacy(ctx context.Context, r Rule, mappings []Mapping) error {
	if err := ValidateRule(r); err != nil {
		return err
	}
	if r.State != "paused" {
		return errors.New("legacy rule must be paused")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockRuleGraph(ctx, tx); err != nil {
		return err
	}
	if err := s.validateRuleRoute(ctx, tx, r); err != nil {
		return err
	}

	var calendarCount int
	if err := tx.QueryRowContext(ctx, s.query("SELECT COUNT(*) FROM calendars WHERE id IN (?, ?)"), r.SourceCalendarID, r.TargetCalendarID).Scan(&calendarCount); err != nil {
		return fmt.Errorf("check legacy calendars: %w", err)
	}
	if calendarCount != 2 {
		return errors.New("legacy source and target calendars must already be connected and discovered")
	}
	for _, mapping := range mappings {
		if mapping.RuleID != r.ID {
			return fmt.Errorf("legacy mapping %q belongs to another rule", mapping.ID)
		}
		if mapping.SourceEventID == "" || mapping.TargetEventID == "" {
			return fmt.Errorf("legacy mapping %q is missing a source or target event id", mapping.ID)
		}
		var existing int
		if err := tx.QueryRowContext(ctx, s.query("SELECT COUNT(*) FROM event_mappings WHERE target_event_id=?"), mapping.TargetEventID).Scan(&existing); err != nil {
			return fmt.Errorf("check legacy target mapping: %w", err)
		}
		if existing != 0 {
			return fmt.Errorf("legacy target event %q is already mapped by another rule", mapping.TargetEventID)
		}
	}
	if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO sync_rules(id, source_calendar_id, target_calendar_id, state,
		interval_seconds, lookback_days, lookahead_days, recurrence_mode, copy_attendees, notification_policy,
		next_run_at, consecutive_failures, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.SourceCalendarID, r.TargetCalendarID, r.State, r.IntervalSeconds, r.LookbackDays, r.LookaheadDays,
		r.RecurrenceMode, r.CopyAttendees, r.NotificationPolicy, r.NextRunAt, r.ConsecutiveFailures, r.CreatedAt, r.UpdatedAt); err != nil {
		return fmt.Errorf("create legacy rule: %w", err)
	}
	for _, mapping := range mappings {
		token, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO event_mappings(id, rule_id, object_kind, source_event_id,
			source_series_id, original_start, target_event_id, target_series_id, content_hash, last_seen_at,
			reconciliation_state, incarnation) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			mapping.ID, mapping.RuleID, mapping.ObjectKind, mapping.SourceEventID, mapping.SourceSeriesID,
			mapping.OriginalStart, mapping.TargetEventID, mapping.TargetSeriesID, mapping.ContentHash,
			mapping.LastSeenAt, mapping.ReconciliationState, "new:"+token.String()); err != nil {
			return fmt.Errorf("create legacy mapping %q: %w", mapping.SourceEventID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy import: %w", err)
	}
	return nil
}

func (s *Store) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, s.timeProjection(`SELECT id, source_calendar_id, target_calendar_id, state, interval_seconds,
		lookback_days, lookahead_days, recurrence_mode, copy_attendees, notification_policy, next_run_at,
		consecutive_failures, created_at, updated_at, sync_mode FROM sync_rules ORDER BY created_at`, "next_run_at", "created_at", "updated_at"))
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()
	var result []Rule
	for rows.Next() {
		var r Rule
		next, created, updated := s.scanTime(), s.scanTime(), s.scanTime()
		if err := rows.Scan(&r.ID, &r.SourceCalendarID, &r.TargetCalendarID, &r.State, &r.IntervalSeconds,
			&r.LookbackDays, &r.LookaheadDays, &r.RecurrenceMode, &r.CopyAttendees, &r.NotificationPolicy, &next,
			&r.ConsecutiveFailures, &created, &updated, &r.SyncMode); err != nil {
			return nil, err
		}
		r.CreatedAt, r.UpdatedAt = created.Time, updated.Time
		if next.Valid {
			r.NextRunAt = &next.Time
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) RuleByID(ctx context.Context, id string) (Rule, error) {
	row := s.db.QueryRowContext(ctx, s.query(s.timeProjection(`SELECT id, source_calendar_id, target_calendar_id, state, interval_seconds,
		lookback_days, lookahead_days, recurrence_mode, copy_attendees, notification_policy, next_run_at,
		consecutive_failures, created_at, updated_at, sync_mode FROM sync_rules WHERE id=?`, "next_run_at", "created_at", "updated_at")), id)
	var r Rule
	next, created, updated := s.scanTime(), s.scanTime(), s.scanTime()
	if err := row.Scan(&r.ID, &r.SourceCalendarID, &r.TargetCalendarID, &r.State, &r.IntervalSeconds, &r.LookbackDays,
		&r.LookaheadDays, &r.RecurrenceMode, &r.CopyAttendees, &r.NotificationPolicy, &next, &r.ConsecutiveFailures, &created, &updated, &r.SyncMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, fmt.Errorf("scan rule: %w", err)
	}
	r.CreatedAt, r.UpdatedAt = created.Time, updated.Time
	if next.Valid {
		r.NextRunAt = &next.Time
	}
	return r, nil
}

func (s *Store) SetRuleState(ctx context.Context, id, state string, nextRunAt *time.Time, updatedAt time.Time) error {
	if state != "paused" && state != "enabled" {
		return errors.New("rule state must be paused or enabled")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockRuleGraph(ctx, tx); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE sync_rules SET state=?,next_run_at=?,updated_at=? WHERE id=? AND state<>'archived' AND
		(? <> 'enabled' OR NOT EXISTS (SELECT 1 FROM sync_jobs WHERE rule_id=sync_rules.id AND (state='unknown' OR (state='running' AND stop_requested=TRUE))) AND NOT EXISTS (SELECT 1 FROM exclusive_dispatch_reservations WHERE released_at IS NULL) AND sync_mode='direct' AND EXISTS (SELECT 1 FROM calendars source JOIN calendars target ON target.id=sync_rules.target_calendar_id
		WHERE source.id=sync_rules.source_calendar_id AND source.enabled=TRUE AND target.enabled=TRUE))`), state, nextRunAt, updatedAt, id, state)
	if err != nil {
		return fmt.Errorf("set rule state: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// HideDeclinedEvents returns the service-wide calendar display preference.
func (s *Store) HideDeclinedEvents(ctx context.Context) (bool, error) {
	var hidden bool
	err := s.db.QueryRowContext(ctx, "SELECT hide_declined FROM calendar_display_settings WHERE id=1").Scan(&hidden)
	return hidden, err
}

func (s *Store) SetHideDeclinedEvents(ctx context.Context, hidden bool) error {
	result, err := s.db.ExecContext(ctx, s.query("UPDATE calendar_display_settings SET hide_declined=? WHERE id=1"), hidden)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}
