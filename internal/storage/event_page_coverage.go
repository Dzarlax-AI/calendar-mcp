package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrExplicitCachedPageScope identifies a default selection that must be narrowed.
var ErrExplicitCachedPageScope = errors.New("select at most 16 explicit calendar IDs")

// CachedPageGrant is the local authority and eligibility checked around each
// read. It is used only for cursor binding and never returned to clients.
type CachedPageGrant struct {
	CalendarID, ConnectionID, Provider, Revision, Receipt string
	LegacyGeneration                                      int64
	Read                                                  bool
}

func IsCachedPageProvider(provider string) bool {
	return provider == "google" || provider == "apple" || provider == "microsoft"
}

// CaptureCachedPageCoverage reads the bounded selection without loading events.
func (s *Store) CaptureCachedPageCoverage(ctx context.Context, req CachedPageRequest) ([]CachedPageGrant, error) {
	if err := validateCachedPageRequest(req); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, cachedPageReadTxOptions(s.dialect))
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, grants, err := s.cachedPageCoverage(ctx, tx, req, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return grants, tx.Commit()
}

func (s *Store) cachedPageCoverage(ctx context.Context, tx *sql.Tx, req CachedPageRequest, now time.Time) ([]CachedPageSource, []CachedPageGrant, error) {
	var args []any
	scope := ""
	if len(req.CalendarIDs) > 0 {
		scope = " AND cal.id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(req.CalendarIDs)), ",") + ")"
		for _, id := range req.CalendarIDs {
			args = append(args, id)
		}
	}
	length := func(expr string) string {
		if s.dialect == DialectPostgres {
			return "octet_length(" + expr + ")"
		}
		return "length(CAST(" + expr + " AS BLOB))"
	}
	bounded := func(expr string, limit int) string {
		return fmt.Sprintf("CASE WHEN %s<=%d THEN %s ELSE NULL END", length(expr), limit, expr)
	}
	query := fmt.Sprintf(`SELECT %s,%s,conn.provider,%s,%s,
		COALESCE(ss.generation,-1), CASE WHEN ss.status IN ('ready','failed','degraded','pending','syncing','parked') THEN ss.status WHEN ss.status IS NULL THEN 'pending' ELSE 'unknown' END,
		CASE WHEN ss.last_error_code IN ('auth','permission') THEN ss.last_error_code ELSE '' END,
		ss.last_success_at,ss.next_sync_at,ss.window_start,ss.window_end
		FROM calendars cal JOIN connections conn ON conn.id=cal.connection_id
		LEFT JOIN calendar_sync_state ss ON ss.calendar_id=cal.id
		WHERE cal.enabled=TRUE AND cal.can_read=TRUE AND conn.status='connected'
		AND conn.provider IN ('google','apple','microsoft')%s ORDER BY cal.id LIMIT 17`,
		bounded("cal.id", cachedPageMaxIDBytes), bounded("conn.id", cachedPageMaxIDBytes),
		bounded("conn.authorization_revision", 128), bounded("cal.detail_receipt", 1024), scope)
	query = s.timeProjection(query, "ss.last_success_at", "ss.next_sync_at", "ss.window_start", "ss.window_end")
	rows, err := tx.QueryContext(ctx, s.query(query), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	sources := make([]CachedPageSource, 0, 16)
	grants := make([]CachedPageGrant, 0, 16)
	for rows.Next() {
		if len(sources) == 16 {
			return nil, nil, ErrExplicitCachedPageScope
		}
		var grant CachedPageGrant
		var generation int64
		var status, denial string
		last, next, start, end := s.scanTime(), s.scanTime(), s.scanTime(), s.scanTime()
		if err := rows.Scan(&grant.CalendarID, &grant.ConnectionID, &grant.Provider, &grant.Revision, &grant.Receipt,
			&generation, &status, &denial, &last, &next, &start, &end); err != nil {
			return nil, nil, err
		}
		prefix := grant.Provider + ":" + grant.ConnectionID + ":"
		if grant.ConnectionID == "" || !strings.HasPrefix(grant.CalendarID, prefix) || len(grant.CalendarID) == len(prefix) ||
			!utf8.ValidString(grant.CalendarID) || !utf8.ValidString(grant.Revision) || !utf8.ValidString(grant.Receipt) {
			return nil, nil, ErrCachedPageUnavailable
		}
		authorized := grant.Revision != "" && DetailReceiptRole(grant.Receipt) != ""
		if grant.Provider != "google" {
			// Legacy non-Google rows predate the receipt. Selection changes and
			// known revocations replace it, while the legacy generation fences
			// cursors until a durable proof has been initialized.
			authorized = true
			if grant.Revision == "" || grant.Receipt == "" {
				grant.LegacyGeneration = generation
			}
		}
		source := CachedPageSource{
			CalendarID: grant.CalendarID, Provider: grant.Provider, Status: status,
			ErrorCode: denial, Coverage: "unavailable", Stale: !last.Valid || !next.Valid || !next.Time.After(now),
		}
		if start.Valid && end.Valid {
			source.WindowStart, source.WindowEnd = start.Time, end.Time
		}
		if last.Valid && denial == "" && authorized {
			source.LastSuccessAt = &last.Time
		}
		// Pending replacements and parked calendars have no readable snapshot.
		// A prior completed snapshot may give useful partial results during
		// a refresh or retry. Revocation clears both the proof and last success.
		grant.Read = authorized && denial == "" && last.Valid && start.Valid && end.Valid &&
			(status == "ready" || status == "degraded" || status == "syncing" || status == "failed")
		switch {
		case !grant.Read:
		case req.Start.Before(start.Time) || req.End.After(end.Time):
			source.Coverage = "outside_window"
		case status == "ready":
			source.Coverage = "covered"
		default:
			source.Coverage = "partial"
		}
		sources = append(sources, source)
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(req.CalendarIDs) > 0 && len(sources) != len(req.CalendarIDs) {
		return nil, nil, ErrCachedPageUnavailable
	}
	return sources, grants, nil
}
