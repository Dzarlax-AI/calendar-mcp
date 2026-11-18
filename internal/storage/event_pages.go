package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"calendar-mcp/internal/calendar"
)

var (
	ErrCachedPageUnavailable = errors.New("requested cached calendar scope unavailable")
	ErrInvalidCachedPageData = errors.New("invalid cached page data")
)

const (
	cachedPageMaxPayloadBytes = 32 << 10
	cachedPageMaxIDBytes      = 1024
	cachedPageMaxTimeBytes    = 128
)

// CaptureCachedPageAuthority obtains a bounded metadata-only source snapshot.
// The snapshot is used to bind a continuation before reading event data.
func (s *Store) CaptureCachedPageAuthority(ctx context.Context, req CachedPageRequest) ([]AuthorityTicket, error) {
	req = normalizeCachedPageRange(req)
	if err := validateCachedPageRequest(req); err != nil {
		return nil, err
	}
	if len(req.CalendarIDs) == 0 {
		return []AuthorityTicket{}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire cached page authority connection: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, cachedPageReadTxOptions(s.dialect))
	if err != nil {
		return nil, fmt.Errorf("begin cached page authority snapshot: %w", err)
	}
	defer tx.Rollback()
	_, tickets, _, err := cachedPageMetadata(ctx, s, tx, req, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit cached page authority snapshot: %w", err)
	}
	return tickets, nil
}

// ReadCachedPage reads source metadata and at most page_size+1 projected rows
// from one repeatable-read/read-only transaction on the Store's existing pool.
func (s *Store) ReadCachedPage(ctx context.Context, req CachedPageRequest, after *CachedPagePosition) (page CachedPageRead, err error) {
	req = normalizeCachedPageRange(req)
	if err := validateCachedPageRequest(req); err != nil {
		return page, err
	}
	if after != nil {
		if err := validateCachedPagePosition(req, *after); err != nil {
			return page, err
		}
	}
	page.Rows = []CachedPageRow{}
	page.Sources = []CachedPageSource{}
	page.Tickets = []AuthorityTicket{}
	page.ObservedAt = time.Now().UTC()
	if len(req.CalendarIDs) == 0 {
		return page, nil
	}
	poolStart := time.Now()
	conn, err := s.db.Conn(ctx)
	page.Stats.PoolWaitNS = time.Since(poolStart).Nanoseconds()
	if err != nil {
		return page, fmt.Errorf("acquire cached page reader connection: %w", err)
	}
	defer conn.Close()
	phaseStart := time.Now()
	tx, err := conn.BeginTx(ctx, cachedPageReadTxOptions(s.dialect))
	page.Stats.SQLBeginNS = time.Since(phaseStart).Nanoseconds()
	if err != nil {
		return page, fmt.Errorf("begin cached page snapshot: %w", err)
	}
	defer tx.Rollback()
	phaseStart = time.Now()
	page.Sources, page.Tickets, _, err = cachedPageMetadata(ctx, s, tx, req, page.ObservedAt)
	page.Stats.SQLMetadataNS = time.Since(phaseStart).Nanoseconds()
	if err != nil {
		return page, err
	}
	page.Rows, page.Stats.SQLEventsNS, page.Stats.DecodeNS, page.Stats.RowsDecoded, page.Stats.PayloadBytes, page.Stats.MaxPayloadBytes, err = queryCachedPageEvents(ctx, s, tx, req, after)
	if err != nil {
		return page, err
	}
	phaseStart = time.Now()
	if err := tx.Commit(); err != nil {
		page.Stats.SQLCommitNS = time.Since(phaseStart).Nanoseconds()
		return page, fmt.Errorf("commit cached page snapshot: %w", err)
	}
	page.Stats.SQLCommitNS = time.Since(phaseStart).Nanoseconds()
	return page, nil
}

func normalizeCachedPageRange(req CachedPageRequest) CachedPageRequest {
	req.Start = req.Start.UTC()
	req.End = req.End.UTC()
	return req
}

func cachedPageReadTxOptions(dialect Dialect) *sql.TxOptions {
	opts := &sql.TxOptions{ReadOnly: true}
	if dialect == DialectPostgres {
		opts.Isolation = sql.LevelRepeatableRead
	}
	return opts
}

func validateCachedPageRequest(req CachedPageRequest) error {
	if req.PageSize < 1 || req.PageSize > 50 || !req.End.After(req.Start) || len(req.CalendarIDs) > 16 || req.Start.Year() < 1 || req.End.Year() > 9999 {
		return errors.New("invalid cached page request")
	}
	seen := make(map[string]struct{}, len(req.CalendarIDs))
	for _, id := range req.CalendarIDs {
		if id == "" || len(id) > cachedPageMaxIDBytes || !utf8.ValidString(id) {
			return errors.New("invalid cached page calendar scope")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate cached page calendar scope")
		}
		seen[id] = struct{}{}
		provider, rest, ok := strings.Cut(id, ":")
		connectionID, providerID, found := strings.Cut(rest, ":")
		if !ok || !found || provider != "google" || connectionID == "" || providerID == "" {
			return ErrCachedPageUnavailable
		}
	}
	return nil
}

func validateCachedPagePosition(req CachedPageRequest, pos CachedPagePosition) error {
	if pos.EventID == "" || len(pos.EventID) > cachedPageMaxIDBytes || !utf8.ValidString(pos.EventID) || pos.CalendarID == "" || len(pos.CalendarID) > cachedPageMaxIDBytes || !utf8.ValidString(pos.CalendarID) || len(pos.StartAt) > cachedPageMaxTimeBytes || len(pos.StartDate) > cachedPageMaxTimeBytes || !utf8.ValidString(pos.StartAt) || !utf8.ValidString(pos.StartDate) {
		return errors.New("invalid cached page position")
	}
	found := false
	for _, id := range req.CalendarIDs {
		if pos.CalendarID == id {
			found = true
			break
		}
	}
	if !found || (pos.AllDay && (pos.StartDate == "" || pos.StartAt != "")) || (!pos.AllDay && (pos.StartAt == "" || pos.StartDate != "")) {
		return errors.New("invalid cached page position")
	}
	return nil
}

// cachedPageMetadata returns only bounded source proof and freshness metadata.
// Long or malformed proof scalars are replaced with NULL in SQL before the
// driver receives the row, then rejected by the caller.
func cachedPageMetadata(ctx context.Context, s *Store, tx *sql.Tx, req CachedPageRequest, observedAt time.Time) ([]CachedPageSource, []AuthorityTicket, int64, error) {
	if len(req.CalendarIDs) == 0 {
		return []CachedPageSource{}, []AuthorityTicket{}, 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(req.CalendarIDs)), ",")
	length := func(expr string) string {
		if s.dialect == DialectPostgres {
			return "octet_length(" + expr + ")"
		}
		return "length(CAST(" + expr + " AS BLOB))"
	}
	valid := fmt.Sprintf("%s<=%d AND %s<=%d AND %s<=%d AND %s<=%d", length("conn.id"), cachedPageMaxIDBytes, length("cal.provider_calendar_id"), cachedPageMaxIDBytes, length("conn.authorization_revision"), 128, length("cal.detail_receipt"), 1024)
	q := fmt.Sprintf(`SELECT
CASE WHEN %s THEN conn.id ELSE NULL END,
CASE WHEN %s THEN cal.provider_calendar_id ELSE NULL END,
CASE WHEN %s THEN conn.authorization_revision ELSE NULL END,
CASE WHEN %s THEN cal.detail_receipt ELSE NULL END,
ss.generation, CASE WHEN ss.status IN ('ready','failed','degraded','pending','syncing','parked') THEN ss.status ELSE 'unknown' END,
ss.last_success_at, ss.next_sync_at, ss.window_start, ss.window_end,
CASE WHEN ss.last_error_code IN ('auth','permission') THEN ss.last_error_code ELSE '' END,
CASE WHEN %s THEN 0 ELSE 1 END
FROM calendars cal JOIN connections conn ON conn.id=cal.connection_id
LEFT JOIN calendar_sync_state ss ON ss.calendar_id=cal.id
WHERE cal.id IN (%s) AND conn.provider='google' AND conn.status='connected' AND cal.can_read=? AND cal.enabled=TRUE ORDER BY cal.id`, valid, valid, valid, valid, valid, placeholders)
	args := make([]any, 0, len(req.CalendarIDs)+1)
	for _, id := range req.CalendarIDs {
		args = append(args, id)
	}
	args = append(args, true)
	start := time.Now()
	rows, err := tx.QueryContext(ctx, s.query(s.timeProjection(q, "ss.last_success_at", "ss.next_sync_at", "ss.window_start", "ss.window_end")), args...)
	elapsed := time.Since(start).Nanoseconds()
	if err != nil {
		return nil, nil, elapsed, fmt.Errorf("query cached page source metadata: %w", err)
	}
	sources := make([]CachedPageSource, 0, len(req.CalendarIDs))
	tickets := make([]AuthorityTicket, 0, len(req.CalendarIDs))
	found := make(map[string]bool, len(req.CalendarIDs))
	for rows.Next() {
		var connectionID, providerCalendarID, revision, receipt sql.NullString
		var generation sql.NullInt64
		var status, errorCode sql.NullString
		last, next, windowStart, windowEnd := s.scanTime(), s.scanTime(), s.scanTime(), s.scanTime()
		var invalid int
		if err := rows.Scan(&connectionID, &providerCalendarID, &revision, &receipt, &generation, &status, &last, &next, &windowStart, &windowEnd, &errorCode, &invalid); err != nil {
			_ = rows.Close()
			return nil, nil, elapsed, fmt.Errorf("scan cached page source metadata: %w", err)
		}
		if invalid != 0 || !connectionID.Valid || !providerCalendarID.Valid || !revision.Valid || !receipt.Valid || !generation.Valid || !status.Valid || !next.Valid || !windowStart.Valid || !windowEnd.Valid {
			_ = rows.Close()
			return nil, nil, elapsed, ErrCachedPageUnavailable
		}
		calendarID := "google:" + connectionID.String + ":" + providerCalendarID.String
		if !utf8.ValidString(connectionID.String) || !utf8.ValidString(providerCalendarID.String) || len(connectionID.String)+len(providerCalendarID.String)+8 > cachedPageMaxIDBytes || DetailReceiptRole(receipt.String) == "" || revision.String == "" || len(revision.String) > 128 || len(receipt.String) > 1024 {
			_ = rows.Close()
			return nil, nil, elapsed, ErrCachedPageUnavailable
		}
		requested := false
		for _, id := range req.CalendarIDs {
			if id == calendarID {
				requested = true
				break
			}
		}
		if !requested || found[calendarID] || errorCode.Valid && (errorCode.String == "auth" || errorCode.String == "permission") {
			_ = rows.Close()
			return nil, nil, elapsed, ErrCachedPageUnavailable
		}
		if req.Start.Before(windowStart.Time) || req.End.After(windowEnd.Time) {
			_ = rows.Close()
			return nil, nil, elapsed, ErrCachedPageUnavailable
		}
		found[calendarID] = true
		source := CachedPageSource{
			CalendarID: calendarID, Provider: "google", Status: safeCachedStatus(status.String),
			NextSyncAt: &next.Time, WindowStart: windowStart.Time, WindowEnd: windowEnd.Time,
		}
		if last.Valid {
			lastAt := last.Time
			source.LastSuccessAt = &lastAt
		}
		source.Stale = !last.Valid || next.Time.Before(observedAt)
		sources = append(sources, source)
		tickets = append(tickets, AuthorityTicket{ConnectionID: connectionID.String, CalendarID: calendarID, AuthorizationRevision: revision.String, DetailReceipt: receipt.String, Generation: generation.Int64})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, elapsed, fmt.Errorf("iterate cached page source metadata: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, elapsed, fmt.Errorf("close cached page source metadata: %w", err)
	}
	if len(sources) != len(req.CalendarIDs) {
		return nil, nil, elapsed, ErrCachedPageUnavailable
	}
	return sources, tickets, elapsed, nil
}

func safeCachedStatus(status string) string {
	switch status {
	case "ready", "failed", "degraded", "pending", "syncing", "parked":
		return status
	default:
		return "unknown"
	}
}

type cachedPageScalarTime struct {
	time.Time
	Valid bool
}

func (t *cachedPageScalarTime) Scan(value any) error {
	if value == nil {
		t.Valid = false
		return nil
	}
	if parsed, ok := value.(time.Time); ok {
		t.Time, t.Valid = parsed, true
		return nil
	}
	var raw string
	switch value := value.(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	default:
		return ErrInvalidCachedPageData
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 -0700", "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999 -0700", "2006-01-02 15:04:05.999999999"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			t.Time, t.Valid = parsed, true
			return nil
		}
	}
	return ErrInvalidCachedPageData
}

func cachedPageProjection(s *Store) (string, string, string) {
	projection := `json_object('title',json(e.payload_json->'$.title'),'start',json(e.payload_json->'$.start'),'end',json(e.payload_json->'$.end'),'status',json(e.payload_json->'$.status'))`
	summarySize := "length(CAST(summary_json AS BLOB))"
	if s.dialect == DialectPostgres {
		projection = `jsonb_build_object('title',e.payload_json->'title','start',e.payload_json->'start','end',e.payload_json->'end','status',e.payload_json->'status')::text`
		summarySize = "octet_length(summary_json)"
	}
	checks := []string{}
	for _, column := range []string{"e.event_id", "e.calendar_id", "e.start_at", "e.end_at", "e.start_date", "e.end_date"} {
		limit := cachedPageMaxTimeBytes
		if column == "e.event_id" || column == "e.calendar_id" {
			limit = cachedPageMaxIDBytes
		}
		check := fmt.Sprintf("(%s IS NULL OR octet_length(CAST(%s AS TEXT))<=%d)", column, column, limit)
		if s.dialect == DialectSQLite {
			check = fmt.Sprintf("(%s IS NULL OR (typeof(%s)='text' AND length(CAST(%s AS BLOB))<=%d))", column, column, column, limit)
		}
		checks = append(checks, check)
	}
	checks = append(checks, "e.event_id IS NOT NULL", "e.event_id<>''", "e.calendar_id IS NOT NULL", "e.calendar_id<>''")
	guard := strings.Join(checks, " AND ")
	scalarOK := guard
	if s.dialect == DialectSQLite {
		scalarOK = "(" + guard + ") AND json_valid(e.payload_json)"
	}
	projection = fmt.Sprintf("CASE WHEN %s THEN %s ELSE NULL END", scalarOK, projection)
	return scalarOK, projection, summarySize
}

func cachedPageProjectionSuffix(s *Store, scalarOK, projection, summarySize string) string {
	suffix := ""
	if s.dialect == DialectSQLite {
		suffix += fmt.Sprintf(`, projected AS MATERIALIZED (
SELECT e.event_id,e.calendar_id,e.start_key,e.start_at,e.end_at,e.start_date,e.end_date,
(%s) AS scalar_ok,%s AS summary_json FROM selected e)`, scalarOK, projection)
	}
	safe := func(expression string) string { return "CASE WHEN scalar_ok THEN " + expression + " ELSE NULL END" }
	source := "selected"
	if s.dialect == DialectSQLite {
		source = "projected"
	}
	suffix += fmt.Sprintf(` SELECT %s,%s,%s,%s,%s,%s,%s,CASE WHEN scalar_ok AND %s<=%d THEN summary_json ELSE NULL END,CASE WHEN scalar_ok THEN 0 ELSE 1 END
FROM %s ORDER BY start_at IS NULL,start_at,CASE WHEN start_at IS NULL THEN start_date END,event_id,calendar_id`, safe("event_id"), safe("calendar_id"), safe("start_key"), safe("start_at"), safe("end_at"), safe("start_date"), safe("end_date"), summarySize, cachedPageMaxPayloadBytes, source)
	return suffix
}

func cachedPageEventsFallbackQuery(s *Store, req CachedPageRequest, after *CachedPagePosition) (string, []any, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(req.CalendarIDs)), ",")
	args := make([]any, 0, 64)
	for _, id := range req.CalendarIDs {
		args = append(args, id)
	}
	args = append(args, true, "connected", false)
	allDayEnd := req.End.UTC()
	if allDayEnd.Hour() != 0 || allDayEnd.Minute() != 0 || allDayEnd.Second() != 0 || allDayEnd.Nanosecond() != 0 {
		allDayEnd = allDayEnd.AddDate(0, 0, 1)
	}
	overlap := `((e.start_at IS NOT NULL AND e.start_at<? AND e.end_at>?) OR (e.start_at IS NULL AND e.start_date IS NOT NULL AND e.start_date<? AND e.end_date>?))`
	if s.dialect == DialectSQLite {
		overlap = fmt.Sprintf(`e.rowid IN (SELECT rowid FROM cached_events WHERE calendar_id IN (%s) AND deleted=? AND start_at IS NOT NULL AND start_at<? AND end_at>? UNION SELECT rowid FROM cached_events WHERE calendar_id IN (%s) AND deleted=? AND start_date IS NOT NULL AND start_date<? AND end_date>?)`, placeholders, placeholders)
		for _, id := range req.CalendarIDs {
			args = append(args, id)
		}
		args = append(args, false, req.End, req.Start)
		for _, id := range req.CalendarIDs {
			args = append(args, id)
		}
		args = append(args, false, allDayEnd.Format(calendar.DateLayout), req.Start.UTC().Format(calendar.DateLayout))
		// The UNION date arm must remain eligible for the existing covering
		// index. Recheck timed overlap on the outer row before LIMIT so a timed
		// event with a stray date cannot qualify through that arm.
		overlap += ` AND (e.start_at IS NULL OR (e.start_at<? AND e.end_at>?))`
		args = append(args, req.End, req.Start)
	} else {
		args = append(args, req.End, req.Start, allDayEnd.Format(calendar.DateLayout), req.Start.UTC().Format(calendar.DateLayout))
	}
	scalarOK, projection, summarySize := cachedPageProjection(s)
	master := "CASE WHEN json_valid(e.payload_json) THEN json_extract(e.payload_json, '$.instance_kind') ELSE '' END"
	if s.dialect == DialectPostgres {
		master = "e.payload_json->>'instance_kind'"
	}
	startKey := "CAST(e.start_at AS TEXT)"
	selectColumns := fmt.Sprintf(`e.event_id,e.calendar_id,%s AS start_key,e.start_at,e.end_at,e.start_date,e.end_date,
(%s) AS scalar_ok,%s AS summary_json`, startKey, scalarOK, projection)
	if s.dialect == DialectSQLite {
		// Delay scalar guards and summary projection until after the bounded page
		// is selected. The payload stays inside SQLite until its size is checked.
		selectColumns = `e.event_id,e.calendar_id,CAST(e.start_at AS TEXT) AS start_key,e.start_at,e.end_at,e.start_date,e.end_date,e.payload_json`
	}
	query := fmt.Sprintf(`WITH selected AS MATERIALIZED (
SELECT %s
FROM cached_events e JOIN calendars c ON c.id=e.calendar_id JOIN connections conn ON conn.id=c.connection_id
WHERE e.calendar_id IN (%s) AND c.can_read=? AND c.enabled=TRUE AND conn.status=? AND e.deleted=? AND %s`, selectColumns, placeholders, overlap)
	if s.dialect != DialectSQLite {
		query += fmt.Sprintf(` AND COALESCE(%s,'')<>'seriesMaster'`, master)
	}
	if after != nil {
		if after.AllDay {
			query += ` AND e.start_at IS NULL AND (e.start_date>? OR (e.start_date=? AND (e.event_id>? OR (e.event_id=? AND e.calendar_id>?))))`
			args = append(args, after.StartDate, after.StartDate, after.EventID, after.EventID, after.CalendarID)
		} else {
			var key any = after.StartAt
			compare := "CAST(e.start_at AS TEXT)"
			if s.dialect == DialectPostgres {
				parsed, err := time.Parse(time.RFC3339Nano, after.StartAt)
				if err != nil {
					return "", nil, errors.New("invalid cached page cursor time")
				}
				key, compare = parsed.UTC(), "e.start_at"
			}
			query += fmt.Sprintf(` AND (e.start_at IS NULL OR %s>? OR (%s=? AND (e.event_id>? OR (e.event_id=? AND e.calendar_id>?))))`, compare, compare)
			args = append(args, key, key, after.EventID, after.EventID, after.CalendarID)
		}
	}
	if s.dialect == DialectSQLite {
		// The cursor predicate is cheaper than inspecting a candidate's JSON.
		// Masters must still be excluded before applying the limit.
		query += fmt.Sprintf(` AND COALESCE(%s,'')<>'seriesMaster'`, master)
	}
	query += ` ORDER BY e.start_at IS NULL,e.start_at,CASE WHEN e.start_at IS NULL THEN e.start_date END,e.event_id,e.calendar_id LIMIT ?)`
	query += cachedPageProjectionSuffix(s, scalarOK, projection, summarySize)
	args = append(args, req.PageSize+1)
	return s.query(query), args, nil
}

// cachedPageEventsQuery selects bounded SQLite ranges from the covering indexes.
// The caller must validate every requested source in the same read transaction
// before querying events. That check also covers calendar and connection status.
func cachedPageEventsQuery(s *Store, req CachedPageRequest, after *CachedPagePosition) (string, []any, error) {
	if s.dialect != DialectSQLite {
		return cachedPageEventsFallbackQuery(s, req, after)
	}
	scalarOK, projection, summarySize := cachedPageProjection(s)
	var args []any
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(req.CalendarIDs)), ",")
	limit := req.PageSize + 1
	allDayEnd := req.End.UTC()
	if allDayEnd.Hour() != 0 || allDayEnd.Minute() != 0 || allDayEnd.Second() != 0 || allDayEnd.Nanosecond() != 0 {
		allDayEnd = allDayEnd.AddDate(0, 0, 1)
	}
	args = nil
	var ctes, branches []string
	for _, day := range []bool{false, true} {
		if !day && after != nil && after.AllDay {
			continue
		}
		name, index, order, kind := "timed_range", "cached_events_timed_range_idx", "r.start_at,e.event_id,e.calendar_id", 0
		if day {
			name, index, order, kind = "date_range", "cached_events_date_range_idx", "r.start_date,e.event_id,e.calendar_id", 1
		}
		q := fmt.Sprintf(`%s AS MATERIALIZED (SELECT e.rowid AS rid,%d AS is_day,e.start_at,e.start_date,CASE WHEN %d=1 THEN e.start_date END AS sort_date,e.event_id,e.calendar_id FROM cached_events r INDEXED BY %s CROSS JOIN cached_events e ON e.rowid=r.rowid WHERE r.calendar_id IN (%s) AND r.deleted=0`, name, kind, kind, index, placeholders)
		for _, id := range req.CalendarIDs {
			args = append(args, id)
		}
		if day {
			q += ` AND r.start_date IS NOT NULL AND r.start_date<? AND r.end_date>? AND e.start_at IS NULL`
			args = append(args, allDayEnd.Format(calendar.DateLayout), req.Start.UTC().Format(calendar.DateLayout))
			if after != nil && after.AllDay {
				q += ` AND r.start_date>=? AND (e.start_date>? OR (e.start_date=? AND (e.event_id>? OR (e.event_id=? AND e.calendar_id>?))))`
				args = append(args, after.StartDate, after.StartDate, after.StartDate, after.EventID, after.EventID, after.CalendarID)
			}
		} else {
			q += ` AND r.start_at IS NOT NULL AND r.start_at<? AND r.end_at>?`
			args = append(args, req.End, req.Start)
			if after != nil {
				q += ` AND r.start_at>=? AND (CAST(e.start_at AS TEXT)>? OR (CAST(e.start_at AS TEXT)=? AND (e.event_id>? OR (e.event_id=? AND e.calendar_id>?))))`
				args = append(args, after.StartAt, after.StartAt, after.StartAt, after.EventID, after.EventID, after.CalendarID)
			}
		}
		// In JSON text, the value "seriesMaster" must appear literally or use a
		// Unicode escape. Parse possible matches and leave payload validation of
		// selected rows to the guarded projection.
		q += ` AND COALESCE(CASE WHEN typeof(e.payload_json)='text' AND instr(e.payload_json,'seriesMaster')=0 AND instr(e.payload_json,char(92) || 'u')=0 THEN '' WHEN json_valid(e.payload_json) THEN json_extract(e.payload_json, '$.instance_kind') ELSE '' END,'')<>'seriesMaster' ORDER BY ` + order + ` LIMIT ?)`
		args = append(args, limit)
		ctes = append(ctes, q)
		branches = append(branches, "SELECT * FROM "+name)
	}
	ctes = append(ctes, `candidate_keys AS MATERIALIZED (`+strings.Join(branches, ` UNION ALL `)+` ORDER BY is_day,start_at,sort_date,event_id,calendar_id LIMIT ?)`)
	args = append(args, limit)
	ctes = append(ctes, `selected AS MATERIALIZED (SELECT e.event_id,e.calendar_id,CAST(e.start_at AS TEXT) AS start_key,e.start_at,e.end_at,e.start_date,e.end_date,e.payload_json FROM candidate_keys k JOIN cached_events e ON e.rowid=k.rid)`)
	return "WITH " + strings.Join(ctes, ",\n") + cachedPageProjectionSuffix(s, scalarOK, projection, summarySize), args, nil
}

func queryCachedPageEvents(ctx context.Context, s *Store, tx *sql.Tx, req CachedPageRequest, after *CachedPagePosition) ([]CachedPageRow, int64, int64, int, int64, int, error) {
	query, args, err := cachedPageEventsQuery(s, req, after)
	var gateNS int64
	if err == nil && s.dialect == DialectSQLite && after != nil && !after.AllDay {
		// Only a timed continuation compares a native index key with the old
		// text-cast ordering. Numeric timestamps sort before empty text, so this
		// probe can seek just that part of the index. Blobs sort after the text
		// end bound and cannot pass the original timed overlap predicate.
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(req.CalendarIDs)), ",")
		gateArgs := make([]any, 0, len(req.CalendarIDs)+1)
		for _, id := range req.CalendarIDs {
			gateArgs = append(gateArgs, id)
		}
		gateArgs = append(gateArgs, req.Start)
		var found int
		gateStart := time.Now()
		gateErr := tx.QueryRowContext(ctx, `SELECT 1 FROM cached_events INDEXED BY cached_events_timed_range_idx WHERE calendar_id IN (`+placeholders+`) AND deleted=0 AND start_at IS NOT NULL AND start_at < '' AND end_at>? AND typeof(start_at) IN ('integer','real') LIMIT 1`, gateArgs...).Scan(&found)
		gateNS = time.Since(gateStart).Nanoseconds()
		if gateErr == nil {
			query, args, err = cachedPageEventsFallbackQuery(s, req, after)
		} else if !errors.Is(gateErr, sql.ErrNoRows) {
			err = fmt.Errorf("check cached event timestamp types: %w", gateErr)
		}
	}
	if err != nil {
		return nil, gateNS, 0, 0, 0, 0, err
	}
	start := time.Now()
	rows, err := tx.QueryContext(ctx, query, args...)
	sqlNS := gateNS + time.Since(start).Nanoseconds()
	if err != nil {
		return nil, sqlNS, 0, 0, 0, 0, fmt.Errorf("query cached event page: %w", err)
	}
	result := make([]CachedPageRow, 0, req.PageSize+1)
	var decodeNS, payloadBytes int64
	maxPayload, decoded := 0, 0
	for {
		nextStart := time.Now()
		hasRow := rows.Next()
		sqlNS += time.Since(nextStart).Nanoseconds()
		if !hasRow {
			break
		}
		var id, calendarID, key, startDate, endDate sql.NullString
		var startAt, endAt cachedPageScalarTime
		var raw []byte
		var invalid int
		scanStart := time.Now()
		err = rows.Scan(&id, &calendarID, &key, &startAt, &endAt, &startDate, &endDate, &raw, &invalid)
		sqlNS += time.Since(scanStart).Nanoseconds()
		if err != nil {
			_ = rows.Close()
			return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, fmt.Errorf("scan cached event page: %w", err)
		}
		decodeStart := time.Now()
		if invalid != 0 || !id.Valid || !calendarID.Valid || (startAt.Valid && !key.Valid) || (startAt.Valid != endAt.Valid) {
			_ = rows.Close()
			return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, ErrInvalidCachedPageData
		}
		position := CachedPagePosition{AllDay: !startAt.Valid, EventID: id.String, CalendarID: calendarID.String}
		if position.AllDay {
			if !startDate.Valid || !endDate.Valid || startDate.String == "" || endDate.String == "" || len(startDate.String) > cachedPageMaxTimeBytes || len(endDate.String) > cachedPageMaxTimeBytes {
				_ = rows.Close()
				return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, ErrInvalidCachedPageData
			}
			startDay, startErr := time.Parse(calendar.DateLayout, startDate.String)
			endDay, endErr := time.Parse(calendar.DateLayout, endDate.String)
			if startErr != nil || endErr != nil || !endDay.After(startDay) || !utf8.ValidString(startDate.String) || !utf8.ValidString(endDate.String) {
				_ = rows.Close()
				return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, ErrInvalidCachedPageData
			}
			position.StartDate = startDate.String
		} else if s.dialect == DialectPostgres {
			position.StartAt = startAt.Time.UTC().Format(time.RFC3339Nano)
		} else {
			position.StartAt = key.String
		}
		if position.StartAt != "" && (len(position.StartAt) > cachedPageMaxTimeBytes || !utf8.ValidString(position.StartAt)) {
			_ = rows.Close()
			return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, ErrInvalidCachedPageData
		}
		var summary CachedPageSummary
		if raw == nil {
			summary = CachedPageSummary{Kind: "oversized", EventID: id.String, CalendarID: calendarID.String, Reason: "summary_projection_too_large"}
			if position.AllDay {
				summary.Start.Date, summary.End.Date = startDate.String, endDate.String
			} else {
				summary.Start.DateTime = startAt.Time.UTC().Format(time.RFC3339Nano)
				summary.End.DateTime = endAt.Time.UTC().Format(time.RFC3339Nano)
			}
		} else {
			if !utf8.Valid(raw) {
				_ = rows.Close()
				return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, ErrInvalidCachedPageData
			}
			var fields struct {
				Title  string             `json:"title"`
				Start  calendar.EventTime `json:"start"`
				End    calendar.EventTime `json:"end"`
				Status string             `json:"status"`
			}
			if err := json.Unmarshal(raw, &fields); err != nil {
				_ = rows.Close()
				return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, fmt.Errorf("decode bounded cached summary: %w", ErrInvalidCachedPageData)
			}
			if calendar.ValidateEventTimeRangeV2(fields.Start, fields.End) != nil {
				_ = rows.Close()
				return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, ErrInvalidCachedPageData
			}
			summary = CachedPageSummary{Kind: "event", EventID: id.String, CalendarID: calendarID.String, Title: fields.Title, Start: fields.Start, End: fields.End, Status: fields.Status}
			decoded++
			payloadBytes += int64(len(raw))
			maxPayload = max(maxPayload, len(raw))
		}
		if len(summary.EventID) > cachedPageMaxIDBytes || len(summary.CalendarID) > cachedPageMaxIDBytes || !utf8.ValidString(summary.EventID) || !utf8.ValidString(summary.CalendarID) {
			_ = rows.Close()
			return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, ErrInvalidCachedPageData
		}
		result = append(result, CachedPageRow{Summary: summary, Position: position})
		decodeNS += time.Since(decodeStart).Nanoseconds()
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, fmt.Errorf("iterate cached event page: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, fmt.Errorf("close cached event page: %w", err)
	}
	return result, sqlNS, decodeNS, decoded, payloadBytes, maxPayload, nil
}
