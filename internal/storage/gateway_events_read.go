package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

type GatewayEventQuery struct {
	CalendarIDs                    []string
	CalendarRef, EventID, SeriesID string
	Window                         SyncWindow
	CivilStart, CivilEnd           string
	ShowDeleted                    bool
	ExcludeIDs                     []string
	IncludeImported                bool
}

type GatewayReadSource struct {
	Grant             GatewayCalendarGrant
	Version           string
	Source            CachedPageSource
	DeletionsComplete bool
}

type GatewayReadItem struct {
	Event                calendar.EventV2
	Oversized, Cancelled bool
}

type GatewayEventRead struct {
	Sources    []GatewayReadSource
	Items      []GatewayReadItem
	Incomplete bool
}

// ReadGatewayEvents reads the accepted grants, versions and indexed event
// rows from one local transaction. Inactive sources never reach the row query.
func (s *Store) ReadGatewayEvents(ctx context.Context, req GatewayEventQuery, now time.Time, metadataOnly bool) (GatewayEventRead, error) {
	var out GatewayEventRead
	tx, err := s.db.BeginTx(ctx, cachedPageReadTxOptions(s.dialect))
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	grants, err := s.gatewayReadGrants(ctx, tx, req, now)
	if err != nil {
		return out, err
	}
	if len(grants) > 16 {
		return out, ErrExplicitCachedPageScope
	}
	if req.CalendarRef == "" && len(req.CalendarIDs) == 0 {
		err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM connections c
		LEFT JOIN gateway_account_checks a ON a.connection_id=c.id AND a.authorization_revision=c.authorization_revision
		WHERE c.status='connected' AND (a.connection_id IS NULL OR a.inventory_revision='' OR (`+gatewayProofDeadline+`)<=?))`),
			now.UnixMilli()).Scan(&out.Incomplete)
		if err != nil {
			return out, err
		}
		if !out.Incomplete {
			out.Incomplete, err = s.gatewayFanOutHasDenial(ctx, tx, req)
			if err != nil {
				return out, err
			}
		}
	} else {
		out.Incomplete = len(grants) < max(1, len(req.CalendarIDs))
	}

	out.Sources = make([]GatewayReadSource, 0, len(grants))
	bytes := 0
	for _, grant := range grants {
		if grant.Class == GatewayMetadataOnly {
			out.Incomplete = true
			continue
		}
		if req.EventID != "" {
			item, receipt, err := s.gatewayReadWritePoint(ctx, tx, grant, req.EventID, now)
			if err != nil {
				return out, err
			}
			if receipt != "" {
				out.Sources = append(out.Sources, GatewayReadSource{Grant: grant, Version: "write:" + receipt,
					Source: CachedPageSource{CalendarID: grant.Metadata.ID, Provider: grant.Metadata.Provider, Status: "ready", Coverage: "covered"}})
				if !metadataOnly {
					out.Items = append(out.Items, item)
				}
				continue
			}
		}

		readSource, err := s.gatewayReadSource(ctx, tx, req, grant, now)
		if err != nil {
			return out, err
		}
		out.Sources = append(out.Sources, readSource)
		out.Incomplete = out.Incomplete || readSource.Source.Coverage != "covered"
		if readSource.Version == "" || metadataOnly {
			continue
		}
		items, err := s.gatewayReadRows(ctx, tx, req, grant, now, &bytes, maxGatewayEvents-len(out.Items))
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, items...)
	}
	return out, tx.Commit()
}

func (s *Store) gatewayReadGrants(ctx context.Context, tx *sql.Tx, req GatewayEventQuery, now time.Time) ([]GatewayCalendarGrant, error) {
	if req.CalendarRef != "" {
		return s.gatewayGrantByReference(ctx, tx, req.CalendarRef, now)
	}
	if len(req.CalendarIDs) > 0 {
		var out []GatewayCalendarGrant
		for _, id := range req.CalendarIDs {
			grants, err := s.gatewayCalendarGrants(ctx, tx, now, id)
			if err != nil {
				return nil, err
			}
			out = append(out, grants...)
		}
		return out, nil
	}
	grants, err := s.gatewayCalendarGrants(ctx, tx, now, "")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(grants, func(g GatewayCalendarGrant) bool {
		_, tail, _ := strings.Cut(g.Metadata.ID, ":")
		_, raw, _ := strings.Cut(tail, ":")
		return gatewayFanOutExcluded(req, g.Metadata.ID, g.Metadata.Provider, raw)
	}), nil
}

func gatewayFanOutExcluded(req GatewayEventQuery, id, provider, raw string) bool {
	return slices.Contains(req.ExcludeIDs, id) || slices.Contains(req.ExcludeIDs, provider+":"+raw) ||
		!req.IncludeImported && provider == "google" && strings.Contains(raw, "@import.calendar.google.com")
}

// Keep the closed source out of the response without presenting the remaining
// calendars as a complete result.
func (s *Store) gatewayFanOutHasDenial(ctx context.Context, tx *sql.Tx, req GatewayEventQuery) (bool, error) {
	rows, err := tx.QueryContext(ctx, s.query(`SELECT cal.id,c.provider,cal.provider_calendar_id
	FROM gateway_calendar_grants g JOIN calendars cal ON cal.id=g.calendar_id
	JOIN connections c ON c.id=g.connection_id
	WHERE c.status='connected' AND cal.enabled=TRUE AND g.verified_until_ms=0 AND g.grant_revision LIKE ?`),
		gatewayDenialLeasePrefix+"%")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, provider, raw string
		if err := rows.Scan(&id, &provider, &raw); err != nil {
			return false, err
		}
		if !gatewayFanOutExcluded(req, id, provider, raw) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) gatewayReadSource(ctx context.Context, tx *sql.Tx, req GatewayEventQuery, grant GatewayCalendarGrant, now time.Time) (GatewayReadSource, error) {
	out := GatewayReadSource{Grant: grant, Source: CachedPageSource{CalendarID: grant.Metadata.ID, Provider: grant.Metadata.Provider,
		Status: "pending", Coverage: "unavailable", Stale: true}}
	var observed int64
	var start, end, zone, verifiedSeries string
	err := tx.QueryRowContext(ctx, s.query(`SELECT p.content_revision,p.observed_ms,p.window_start,p.window_end,c.timezone,p.deletions_complete,p.verified_cancellation_series
	FROM gateway_event_publications p JOIN calendar_sync_state ss ON ss.calendar_id=p.calendar_id
	JOIN calendars c ON c.id=p.calendar_id
	WHERE p.calendar_id=? AND p.grant_revision=?
	AND p.mutation_epoch=COALESCE((SELECT epoch FROM projection_mutation_guards g WHERE g.calendar_id=p.calendar_id),0)
	AND ss.status IN ('ready','syncing','failed') AND COALESCE(ss.last_error_code,'') NOT IN ('auth','permission')
	AND NOT EXISTS (SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=p.calendar_id)`),
		grant.Metadata.ID, grant.Revision).Scan(&out.Version, &observed, &start, &end, &zone, &out.DeletionsComplete, &verifiedSeries)
	if errors.Is(err, sql.ErrNoRows) {
		var pending bool
		err := tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=?)`),
			grant.Metadata.ID).Scan(&pending)
		if err != nil {
			return out, err
		}
		if pending {
			out.Source.ErrorCode = "cache_pending"
		}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	at := time.UnixMilli(observed).UTC()
	out.Source.WindowStart, err = time.Parse(gatewayInstantLayout, start)
	if err != nil {
		return out, err
	}
	out.Source.WindowEnd, err = time.Parse(gatewayInstantLayout, end)
	if err != nil {
		return out, err
	}
	out.Source.LastSuccessAt = &at
	if !now.Before(at.Add(GatewayAuthorityLifetime)) {
		out.Version = ""
		out.Source.Status = "stale"
		return out, nil
	}
	out.Source.Status, out.Source.Coverage, out.Source.Stale = "ready", "covered", false
	var activeReceipt int64
	var awaitingWrite int
	err = tx.QueryRowContext(ctx, s.query(`SELECT COALESCE(MAX(p.confirmed_ms),0),
		COALESCE(MAX(CASE WHEN NOT p.settled THEN 1 ELSE 0 END),0) FROM gateway_write_points p
	WHERE p.calendar_id=? AND p.grant_revision=? AND p.confirmed_ms>? AND (NOT p.settled OR p.deleted)
	AND p.mutation_epoch=COALESCE((SELECT g.epoch FROM projection_mutation_guards g WHERE g.calendar_id=p.calendar_id),0)`),
		grant.Metadata.ID, grant.Revision, now.Add(-GatewayAuthorityLifetime).UnixMilli()).Scan(&activeReceipt, &awaitingWrite)
	if err != nil {
		return out, err
	}
	if activeReceipt != 0 {
		// The same provider snapshot can expose different rows when its
		// receipt expires. Invalidate cursors across that boundary.
		out.Version += ":write:" + strconv.FormatInt(activeReceipt, 10)
	}
	if awaitingWrite != 0 {
		out.Source.Coverage = "partial"
		out.Source.ErrorCode = "cache_pending"
	}

	if req.SeriesID != "" {
		var hasSeries bool
		series := gatewayEventID(grant.Metadata.Provider, req.SeriesID)
		err := tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM gateway_event_rows WHERE calendar_id=?
		AND (series_id=? OR event_id=? AND (is_master OR series_id<>'')))`), grant.Metadata.ID, series, series).Scan(&hasSeries)
		if err != nil {
			return out, err
		}
		if !hasSeries {
			out.Source.Coverage = "partial"
		}
		if req.ShowDeleted && !out.DeletionsComplete && hasSeries {
			err := tx.QueryRowContext(ctx, s.query(`SELECT COALESCE((SELECT NULLIF(e.series_id,'') FROM gateway_event_rows e
			WHERE e.calendar_id=? AND e.event_id=?),?)`), grant.Metadata.ID, series, series).Scan(&series)
			if err != nil {
				return out, err
			}
			var ids map[string]string
			if err := json.Unmarshal([]byte(verifiedSeries), &ids); err != nil {
				return out, err
			}
			seriesZone, ok := ids[series]
			out.DeletionsComplete = ok
			if seriesZone != "" {
				zone = seriesZone
			}
		}
	}

	if req.Window.Start.Before(out.Source.WindowStart) || req.Window.End.After(out.Source.WindowEnd) ||
		req.CivilStart != "" && !civilRangeCovered(grant.Metadata.Provider, req.CivilStart, req.CivilEnd, zone, out.Source.WindowStart, out.Source.WindowEnd) ||
		req.ShowDeleted && !out.DeletionsComplete {
		out.Source.Coverage = "partial"
	}
	return out, nil
}

func (s *Store) gatewayReadRows(ctx context.Context, tx *sql.Tx, req GatewayEventQuery, grant GatewayCalendarGrant, now time.Time, bytes *int, limit int) ([]GatewayReadItem, error) {
	query := `SELECT e.event_id,e.is_cancelled,e.oversized,e.payload_json FROM gateway_event_rows e WHERE e.calendar_id=?
	AND NOT EXISTS(SELECT 1 FROM gateway_write_points w WHERE w.calendar_id=e.calendar_id AND w.event_id=e.event_id AND w.grant_revision=? AND w.confirmed_ms>?
	AND (NOT w.settled OR w.deleted AND NOT e.is_cancelled) AND w.mutation_epoch=COALESCE((SELECT g.epoch FROM projection_mutation_guards g WHERE g.calendar_id=e.calendar_id),0))`
	args := []any{grant.Metadata.ID, grant.Revision, now.Add(-GatewayAuthorityLifetime).UnixMilli()}
	if req.EventID != "" {
		query += ` AND event_id=?`
		args = append(args, gatewayEventID(grant.Metadata.Provider, req.EventID))
	} else {
		query += ` AND NOT is_master AND ((time_kind='timed' AND range_start<? AND range_end>?) OR
		(time_kind='date' AND range_start<? AND range_end>?))`
		args = append(args, gatewayInstant(req.Window.End), gatewayInstant(req.Window.Start), req.CivilEnd, req.CivilStart)
		if req.SeriesID != "" {
			query += ` AND series_id=COALESCE((SELECT NULLIF(e.series_id,'') FROM gateway_event_rows e
			WHERE e.calendar_id=? AND e.event_id=?),?)`
			series := gatewayEventID(grant.Metadata.Provider, req.SeriesID)
			args = append(args, grant.Metadata.ID, series, series)
		}
		if !req.ShowDeleted {
			query += ` AND NOT is_cancelled`
		}
	}
	query += ` LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, s.query(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GatewayReadItem
	for rows.Next() {
		var id, payload string
		var cancelled, oversized bool
		if err := rows.Scan(&id, &cancelled, &oversized, &payload); err != nil {
			return nil, err
		}
		*bytes += len(payload)
		if len(out) >= limit || *bytes > maxGatewayReadBytes || len(payload) > maxGatewayPayload {
			return nil, ErrGatewayQueryTooLarge
		}
		item := GatewayReadItem{Cancelled: cancelled, Oversized: oversized}
		if err := json.Unmarshal([]byte(payload), &item.Event); err != nil {
			return nil, err
		}
		if item.Event.ID != id || item.Event.CalendarID != grant.Metadata.ID {
			return nil, ErrAuthorityChanged
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
