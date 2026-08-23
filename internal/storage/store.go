package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
)

var (
	ErrNotFound               = errors.New("storage record not found")
	ErrSchemaMismatch         = errors.New("storage schema version mismatch")
	ErrOAuthNotConsumable     = errors.New("oauth attempt is expired, consumed, or missing")
	ErrConnectionInUse        = errors.New("connection is referenced by a sync rule")
	ErrRuleCycle              = errors.New("sync rule would create a cycle")
	ErrJobLeaseLost           = errors.New("sync job lease is no longer owned by this worker attempt")
	ErrCalendarSyncActive     = errors.New("calendar sync has an active lease")
	ErrCalendarSyncIneligible = errors.New("calendar sync calendar is not readable on a connected connection")
	ErrInvalidSyncCode        = errors.New("calendar sync error code is invalid")
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrations embed.FS

type Store struct {
	db      *sql.DB
	dialect Dialect
}

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
	return &Store{db: db, dialect: dialect}, nil
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
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
	return nil
}

func (s *Store) query(q string) string {
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
	if c.AccountFingerprint == "" {
		c.AccountFingerprint = "pending:" + c.ID
	}
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO connections
		(id, provider, account_fingerprint, display_name, status, encrypted_credentials, credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		c.ID, c.Provider, c.AccountFingerprint, c.DisplayName, c.Status, c.EncryptedCredentials, c.CredentialVersion, c.ScopesJSON,
		c.LastVerifiedAt, nullString(c.LastErrorCode), c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create connection: %w", err)
	}
	return nil
}

func (s *Store) ConnectionByProvider(ctx context.Context, provider string) (Connection, error) {
	row := s.db.QueryRowContext(ctx, s.query(`SELECT id, provider, account_fingerprint, display_name, status, encrypted_credentials,
		credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at FROM connections WHERE provider = ?`), provider)
	return scanConnection(row)
}

func (s *Store) ConnectionByID(ctx context.Context, id string) (Connection, error) {
	row := s.db.QueryRowContext(ctx, s.query(`SELECT id, provider, account_fingerprint, display_name, status, encrypted_credentials,
		credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at FROM connections WHERE id = ?`), id)
	return scanConnection(row)
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

// PersistConnectionCredentials stores a refreshed provider token without
// changing the connection's verification status or error metadata.
func (s *Store) PersistConnectionCredentials(ctx context.Context, id string, encrypted []byte, version int, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE connections SET encrypted_credentials=?, credential_version=?, updated_at=? WHERE id=?`), encrypted, version, updatedAt, id)
	if err != nil {
		return fmt.Errorf("persist connection credentials: %w", err)
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
	res, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET status=?, last_verified_at=?, last_error_code=?, updated_at=? WHERE id=?`),
		status, verifiedAt, nullString(errorCode), verifiedAt, id)
	if err != nil {
		return fmt.Errorf("update connection verification: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	if status == "connected" {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state
			SET status=?, next_sync_at=?, last_error_code=NULL, updated_at=?
			WHERE calendar_id IN (SELECT id FROM calendars WHERE connection_id=?)
				AND status=? AND (lease_until IS NULL OR lease_until<=?)`), "pending", verifiedAt, verifiedAt, id, "parked", verifiedAt); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, provider, account_fingerprint, display_name, status, encrypted_credentials,
		credential_version, scopes_json, last_verified_at, last_error_code, created_at, updated_at FROM connections ORDER BY provider`)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	defer rows.Close()
	var result []Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanConnection(row rowScanner) (Connection, error) {
	var c Connection
	var verified sql.NullTime
	var errorCode sql.NullString
	if err := row.Scan(&c.ID, &c.Provider, &c.AccountFingerprint, &c.DisplayName, &c.Status, &c.EncryptedCredentials,
		&c.CredentialVersion, &c.ScopesJSON, &verified, &errorCode, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c, ErrNotFound
		}
		return c, fmt.Errorf("scan connection: %w", err)
	}
	if verified.Valid {
		c.LastVerifiedAt = &verified.Time
	}
	c.LastErrorCode = errorCode.String
	return c, nil
}

func (s *Store) UpsertCalendar(ctx context.Context, c Calendar) error {
	q := `INSERT INTO calendars(id, connection_id, provider_calendar_id, name, timezone, can_read, can_write, supports_recurrence, discovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(connection_id, provider_calendar_id) DO UPDATE SET
		name=excluded.name, timezone=excluded.timezone, can_read=excluded.can_read, can_write=excluded.can_write,
		supports_recurrence=excluded.supports_recurrence, discovered_at=excluded.discovered_at`
	_, err := s.db.ExecContext(ctx, s.query(q), c.ID, c.ConnectionID, c.ProviderCalendarID, c.Name, c.Timezone,
		c.CanRead, c.CanWrite, c.SupportsRecurrence, c.DiscoveredAt)
	if err != nil {
		return fmt.Errorf("upsert calendar: %w", err)
	}
	return nil
}

func (s *Store) ListCalendars(ctx context.Context, connectionID string) ([]Calendar, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT id, connection_id, provider_calendar_id, name, timezone,
		can_read, can_write, supports_recurrence, discovered_at FROM calendars WHERE connection_id = ? ORDER BY name`), connectionID)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	defer rows.Close()
	var result []Calendar
	for rows.Next() {
		var c Calendar
		if err := rows.Scan(&c.ID, &c.ConnectionID, &c.ProviderCalendarID, &c.Name, &c.Timezone, &c.CanRead, &c.CanWrite, &c.SupportsRecurrence, &c.DiscoveredAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) ListAllCalendars(ctx context.Context) ([]Calendar, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, connection_id, provider_calendar_id, name, timezone,
		can_read, can_write, supports_recurrence, discovered_at FROM calendars ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list all calendars: %w", err)
	}
	defer rows.Close()
	var result []Calendar
	for rows.Next() {
		var c Calendar
		if err := rows.Scan(&c.ID, &c.ConnectionID, &c.ProviderCalendarID, &c.Name, &c.Timezone, &c.CanRead, &c.CanWrite, &c.SupportsRecurrence, &c.DiscoveredAt); err != nil {
			return nil, err
		}
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

func ValidateRule(r Rule) error {
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
		next_run_at, consecutive_failures, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.SourceCalendarID, r.TargetCalendarID, r.State, r.IntervalSeconds, r.LookbackDays, r.LookaheadDays,
		r.RecurrenceMode, r.CopyAttendees, r.NotificationPolicy, r.NextRunAt, r.ConsecutiveFailures, r.CreatedAt, r.UpdatedAt)
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
	if err := tx.QueryRowContext(ctx, s.query("SELECT can_read FROM calendars WHERE id=?"), candidate.SourceCalendarID).Scan(&sourceReadable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("source calendar is not connected")
		}
		return fmt.Errorf("load source calendar: %w", err)
	}
	if !sourceReadable {
		return errors.New("source calendar is not readable")
	}
	if err := tx.QueryRowContext(ctx, s.query("SELECT can_write FROM calendars WHERE id=?"), candidate.TargetCalendarID).Scan(&targetWritable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("target calendar is not connected")
		}
		return fmt.Errorf("load target calendar: %w", err)
	}
	if !targetWritable {
		return errors.New("target calendar is not writable")
	}

	rows, err := tx.QueryContext(ctx, "SELECT source_calendar_id, target_calendar_id FROM sync_rules")
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
		if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO event_mappings(id, rule_id, object_kind, source_event_id,
			source_series_id, original_start, target_event_id, target_series_id, content_hash, last_seen_at,
			reconciliation_state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			mapping.ID, mapping.RuleID, mapping.ObjectKind, mapping.SourceEventID, mapping.SourceSeriesID,
			mapping.OriginalStart, mapping.TargetEventID, mapping.TargetSeriesID, mapping.ContentHash,
			mapping.LastSeenAt, mapping.ReconciliationState); err != nil {
			return fmt.Errorf("create legacy mapping %q: %w", mapping.SourceEventID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy import: %w", err)
	}
	return nil
}

func (s *Store) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_calendar_id, target_calendar_id, state, interval_seconds,
		lookback_days, lookahead_days, recurrence_mode, copy_attendees, notification_policy, next_run_at,
		consecutive_failures, created_at, updated_at FROM sync_rules ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()
	var result []Rule
	for rows.Next() {
		var r Rule
		var next sql.NullTime
		if err := rows.Scan(&r.ID, &r.SourceCalendarID, &r.TargetCalendarID, &r.State, &r.IntervalSeconds,
			&r.LookbackDays, &r.LookaheadDays, &r.RecurrenceMode, &r.CopyAttendees, &r.NotificationPolicy, &next,
			&r.ConsecutiveFailures, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if next.Valid {
			r.NextRunAt = &next.Time
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) RuleByID(ctx context.Context, id string) (Rule, error) {
	row := s.db.QueryRowContext(ctx, s.query(`SELECT id, source_calendar_id, target_calendar_id, state, interval_seconds,
		lookback_days, lookahead_days, recurrence_mode, copy_attendees, notification_policy, next_run_at,
		consecutive_failures, created_at, updated_at FROM sync_rules WHERE id=?`), id)
	var r Rule
	var next sql.NullTime
	if err := row.Scan(&r.ID, &r.SourceCalendarID, &r.TargetCalendarID, &r.State, &r.IntervalSeconds, &r.LookbackDays,
		&r.LookaheadDays, &r.RecurrenceMode, &r.CopyAttendees, &r.NotificationPolicy, &next, &r.ConsecutiveFailures, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, fmt.Errorf("scan rule: %w", err)
	}
	if next.Valid {
		r.NextRunAt = &next.Time
	}
	return r, nil
}

func (s *Store) SetRuleState(ctx context.Context, id, state string, nextRunAt *time.Time, updatedAt time.Time) error {
	if state != "paused" && state != "enabled" {
		return errors.New("rule state must be paused or enabled")
	}
	res, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_rules SET state=?,next_run_at=?,updated_at=? WHERE id=?`), state, nextRunAt, updatedAt, id)
	if err != nil {
		return fmt.Errorf("set rule state: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
