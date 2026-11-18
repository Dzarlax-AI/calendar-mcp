package storage

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

type cachedSourceGrant struct {
	connectionID     string
	provider         string
	revision         string
	receipt          string
	legacyGeneration int64
}

// Google rows awaiting legacy migration have no receipt. Their existing sync
// generation still fences a read that crosses a disable and re-enable.
const legacySelectionGeneration = `CASE WHEN conn.provider='google' AND c.detail_receipt='' THEN COALESCE(ss.generation,-1) ELSE -1 END`

type CachedCalendarCatalogEntry struct {
	ID, Name  string
	Available bool
	Enabled   bool
}

// CachedCalendarCatalog keeps known IDs separate from sources eligible for
// implicit cached reads, including while a connection is being verified.
func (s *Store) CachedCalendarCatalog(ctx context.Context) ([]CachedCalendarCatalogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.name,
		(c.can_read AND conn.status='connected'),c.enabled
		FROM calendars c JOIN connections conn ON conn.id=c.connection_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []CachedCalendarCatalogEntry
	for rows.Next() {
		var entry CachedCalendarCatalogEntry
		if err := rows.Scan(&entry.ID, &entry.Name, &entry.Available, &entry.Enabled); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) captureCachedSourceGrants(ctx context.Context, tx *sql.Tx, placeholders string, ids []any) (map[string]cachedSourceGrant, error) {
	query := fmt.Sprintf(`SELECT c.id, conn.id, conn.provider, conn.authorization_revision, c.detail_receipt, %s
		FROM calendars c JOIN connections conn ON conn.id=c.connection_id LEFT JOIN calendar_sync_state ss ON ss.calendar_id=c.id
		WHERE c.id IN (%s) AND c.can_read=? AND c.enabled=? AND conn.status=?`, legacySelectionGeneration, placeholders)
	rows, err := tx.QueryContext(ctx, s.query(query), append(ids, true, true, "connected")...)
	if err != nil {
		return nil, fmt.Errorf("capture cached source grants: %w", err)
	}
	defer rows.Close()
	grants := make(map[string]cachedSourceGrant, len(ids))
	for rows.Next() {
		var id string
		var grant cachedSourceGrant
		if err := rows.Scan(&id, &grant.connectionID, &grant.provider, &grant.revision, &grant.receipt, &grant.legacyGeneration); err != nil {
			return nil, fmt.Errorf("scan cached source grant: %w", err)
		}
		grants[id] = grant
	}
	return grants, rows.Err()
}

func (s *Store) filterCachedSourceGrants(ctx context.Context, placeholders string, ids []any, initial map[string]cachedSourceGrant, events []calendar.EventV2) ([]calendar.EventV2, []CachedSourceStatus, error) {
	// Match against the current grants even when a source has no sync state.
	// An unchanged unwarmed source still uses the existing pending response.
	query := fmt.Sprintf(`SELECT c.id, conn.id, conn.provider, conn.authorization_revision, c.detail_receipt, %s,
		(conn.provider='google' AND c.google_primary),
		ss.status, %s, ss.last_error_code, %s, %s, %s
		FROM calendars c JOIN connections conn ON conn.id=c.connection_id LEFT JOIN calendar_sync_state ss ON ss.calendar_id=c.id
		WHERE c.id IN (%s) AND c.can_read=? AND c.enabled=? AND conn.status=?`, legacySelectionGeneration, s.cachedStatusTime("ss.last_success_at"), s.cachedStatusTime("ss.next_sync_at"), s.cachedStatusTime("ss.window_start"), s.cachedStatusTime("ss.window_end"), placeholders)
	rows, err := s.db.QueryContext(ctx, s.query(query), append(ids, true, true, "connected")...)
	if err != nil {
		return nil, nil, fmt.Errorf("recheck cached source grants: %w", err)
	}
	defer rows.Close()
	allowed := make(map[string]bool, len(initial))
	statuses := make([]CachedSourceStatus, 0, len(ids))
	now := time.Now().UTC()
	for rows.Next() {
		var status CachedSourceStatus
		var grant cachedSourceGrant
		var state, errorCode sql.NullString
		statusTime := cachedStatusTime{unit: s.sqliteTimeUnit, location: s.sqliteTimeLocation}
		lastSuccess, nextSync, windowStart, windowEnd := statusTime, statusTime, statusTime, statusTime
		if err := rows.Scan(&status.CalendarID, &grant.connectionID, &grant.provider, &grant.revision, &grant.receipt, &grant.legacyGeneration, &status.GooglePrimary,
			&state, &lastSuccess, &errorCode, &nextSync, &windowStart, &windowEnd); err != nil {
			return nil, nil, fmt.Errorf("scan current cached source grant: %w", err)
		}
		if before, ok := initial[status.CalendarID]; !ok || before != grant {
			continue
		}
		allowed[status.CalendarID] = true
		status.Provider = grant.provider
		if state.Valid {
			status.Status, status.ErrorCode = state.String, errorCode.String
			status.WindowStart, status.WindowEnd = windowStart.Time, windowEnd.Time
			if nextSync.Valid {
				status.NextSyncAt = &nextSync.Time
			}
			if lastSuccess.Valid {
				status.LastSuccessAt = &lastSuccess.Time
			}
			status.Stale = !lastSuccess.Valid || nextSync.Time.Before(now)
		} else {
			status.Status, status.Stale = "pending", true
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	seen := make(map[string]bool, len(ids))
	for _, rawID := range ids {
		id := rawID.(string)
		if seen[id] {
			continue
		}
		seen[id] = true
		if !allowed[id] {
			statuses = append(statuses, CachedSourceStatus{CalendarID: id, Status: "unavailable", Stale: true})
		}
	}
	slices.SortFunc(statuses, func(a, b CachedSourceStatus) int { return strings.Compare(a.CalendarID, b.CalendarID) })
	if len(allowed) != len(initial) {
		events = slices.DeleteFunc(events, func(event calendar.EventV2) bool { return !allowed[event.CalendarID] })
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return events, statuses, nil
}

// Cast numeric values to text in SQLite before the driver can infer a unit.
func (s *Store) cachedStatusTime(column string) string {
	if s.dialect == DialectSQLite && s.sqliteTimeUnit != 0 {
		return "CASE WHEN typeof(" + column + ")='integer' THEN CAST(" + column + " AS TEXT) ELSE " + column + " END"
	}
	return column
}

type cachedStatusTime struct {
	time.Time
	Valid    bool
	unit     int64
	location *time.Location
}

func (t *cachedStatusTime) Scan(value any) error {
	if value == nil {
		t.Valid = false
		return nil
	}
	t.Valid = true
	if v, ok := value.(time.Time); ok {
		t.Time = v
		return nil
	}
	var v string
	switch x := value.(type) {
	case string:
		v = x
	case []byte:
		v = string(x)
	case int64:
		v = strconv.FormatInt(x, 10)
	default:
		return fmt.Errorf("unsupported cached status time %T", value)
	}
	if t.unit != 0 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			sec, rem := n/t.unit, n%t.unit
			t.Time = time.Unix(sec, rem*(int64(time.Second)/t.unit)).UTC()
			return nil
		}
	}
	parsed, err := parseLegacySQLiteInstant(v, t.location)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// timeProjection protects selected numeric SQLite timestamps from driver unit guessing.
func (s *Store) timeProjection(query string, columns ...string) string {
	if s.dialect != DialectSQLite || s.sqliteTimeUnit == 0 {
		return query
	}
	from := regexp.MustCompile(`(?i)\sFROM\s`).FindStringIndex(query)
	if from == nil {
		panic("timeProjection: missing FROM")
	}
	selectList, rest := query[:from[0]], query[from[0]:]
	for _, column := range columns {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(column) + `\b`)
		selectList = re.ReplaceAllStringFunc(selectList, func(string) string { return s.cachedStatusTime(column) })
	}
	return selectList + rest
}

func (s *Store) scanTime() cachedStatusTime {
	return cachedStatusTime{unit: s.sqliteTimeUnit, location: s.sqliteTimeLocation}
}
