//go:build ignore

package main

import (
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	sqlite "modernc.org/sqlite"
	"os"
	"sort"
	"strings"
	"time"
)

const cachedPageMaxTimeBytes = 128
const cachedPageMaxIDBytes = 1024
const cachedPageMaxPayloadBytes = 32 << 10
const DialectSQLite = 0
const DialectPostgres = 1

type Store struct{ dialect int }
type CachedPageRequest struct {
	CalendarIDs []string
	Start, End  time.Time
	PageSize    int
}
type CachedPagePosition struct {
	AllDay                                  bool
	StartAt, StartDate, EventID, CalendarID string
}
type built struct {
	Query                      string
	Args                       []any
	Projection, Scalar, Master string
}

func current(s *Store, req CachedPageRequest, after *CachedPagePosition) (built, error) {
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
		args = append(args, false, allDayEnd.Format("2006-01-02"), req.Start.UTC().Format("2006-01-02"))
		// The UNION date arm must remain eligible for the existing covering
		// index. Recheck timed overlap on the outer row before LIMIT so a timed
		// event with a stray date cannot qualify through that arm.
		overlap += ` AND (e.start_at IS NULL OR (e.start_at<? AND e.end_at>?))`
		args = append(args, req.End, req.Start)
	} else {
		args = append(args, req.End, req.Start, allDayEnd.Format("2006-01-02"), req.Start.UTC().Format("2006-01-02"))
	}
	projection := `json_object('title',json(e.payload_json->'$.title'),'start',json(e.payload_json->'$.start'),'end',json(e.payload_json->'$.end'),'status',json(e.payload_json->'$.status'))`
	master := "CASE WHEN json_valid(e.payload_json) THEN json_extract(e.payload_json, '$.instance_kind') ELSE '' END"
	startKey := "CAST(e.start_at AS TEXT)"
	summarySize := "length(CAST(summary_json AS BLOB))"
	if s.dialect == DialectPostgres {
		projection = `jsonb_build_object('title',e.payload_json->'title','start',e.payload_json->'start','end',e.payload_json->'end','status',e.payload_json->'status')::text`
		master = "e.payload_json->>'instance_kind'"
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
WHERE e.calendar_id IN (%s) AND c.can_read=? AND conn.status=? AND e.deleted=? AND %s`, selectColumns, placeholders, overlap)
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
					return built{}, errors.New("invalid cached page cursor time")
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
	if s.dialect == DialectSQLite {
		query += fmt.Sprintf(`, projected AS MATERIALIZED (
SELECT e.event_id,e.calendar_id,e.start_key,e.start_at,e.end_at,e.start_date,e.end_date,
(%s) AS scalar_ok,%s AS summary_json FROM selected e)`, scalarOK, projection)
	}
	safe := func(expression string) string { return "CASE WHEN scalar_ok THEN " + expression + " ELSE NULL END" }
	source := "selected"
	if s.dialect == DialectSQLite {
		source = "projected"
	}
	query += fmt.Sprintf(` SELECT %s,%s,%s,%s,%s,%s,%s,CASE WHEN scalar_ok AND %s<=%d THEN summary_json ELSE NULL END,CASE WHEN scalar_ok THEN 0 ELSE 1 END
FROM %s ORDER BY start_at IS NULL,start_at,CASE WHEN start_at IS NULL THEN start_date END,event_id,calendar_id`, safe("event_id"), safe("calendar_id"), safe("start_key"), safe("start_at"), safe("end_at"), safe("start_date"), safe("end_date"), summarySize, cachedPageMaxPayloadBytes, source)
	args = append(args, req.PageSize+1)
	return built{query, args, projection, scalarOK, master}, nil
}
func candidate(req CachedPageRequest, after *CachedPagePosition, local bool) (built, error) {
	orig, err := current(&Store{}, req, after)
	if err != nil {
		return built{}, err
	}
	ctes := []string{}
	names := []string{}
	args := []any{}
	limit := req.PageSize + 1
	scopes := [][]string{req.CalendarIDs}
	if local {
		scopes = nil
		for _, id := range req.CalendarIDs {
			scopes = append(scopes, []string{id})
		}
	}
	for n, scope := range scopes {
		for _, day := range []bool{false, true} {
			if !day && after != nil && after.AllDay {
				continue
			}
			name := fmt.Sprintf("range_%d_%v", n, day)
			index := "cached_events_timed_range_idx"
			order := "r.start_at,e.event_id,e.calendar_id"
			kind := 0
			if day {
				index = "cached_events_date_range_idx"
				order = "r.start_date,e.event_id,e.calendar_id"
				kind = 1
			}
			q := fmt.Sprintf("%s AS MATERIALIZED (SELECT e.rowid AS rid,%d AS is_day,e.start_at,e.start_date,CASE WHEN %d=1 THEN e.start_date END AS sort_date,e.event_id,e.calendar_id FROM cached_events r INDEXED BY %s CROSS JOIN cached_events e ON e.rowid=r.rowid JOIN calendars c ON c.id=e.calendar_id JOIN connections conn ON conn.id=c.connection_id WHERE r.calendar_id IN (%s) AND r.deleted=0 AND c.can_read=1 AND conn.status='connected'", name, kind, kind, index, strings.TrimSuffix(strings.Repeat("?,", len(scope)), ","))
			for _, id := range scope {
				args = append(args, id)
			}
			if day {
				q += " AND r.start_date IS NOT NULL AND r.start_date<? AND r.end_date>? AND e.start_at IS NULL"
				args = append(args, req.End.Format("2006-01-02"), req.Start.Format("2006-01-02"))
				if after != nil && after.AllDay {
					q += " AND r.start_date>=? AND (e.start_date>? OR (e.start_date=? AND (e.event_id>? OR (e.event_id=? AND e.calendar_id>?))))"
					args = append(args, after.StartDate, after.StartDate, after.StartDate, after.EventID, after.EventID, after.CalendarID)
				}
			} else {
				q += " AND r.start_at IS NOT NULL AND r.start_at<? AND r.end_at>?"
				args = append(args, req.End, req.Start)
				if after != nil {
					q += " AND r.start_at>=? AND (CAST(e.start_at AS TEXT)>? OR (CAST(e.start_at AS TEXT)=? AND (e.event_id>? OR (e.event_id=? AND e.calendar_id>?))))"
					args = append(args, after.StartAt, after.StartAt, after.StartAt, after.EventID, after.EventID, after.CalendarID)
				}
			}
			q += " AND COALESCE(" + orig.Master + ",'')<>'seriesMaster' ORDER BY " + order + " LIMIT ?)"
			args = append(args, limit)
			ctes = append(ctes, q)
			names = append(names, "SELECT * FROM "+name)
		}
	}
	ctes = append(ctes, "candidate_keys AS MATERIALIZED ("+strings.Join(names, " UNION ALL ")+" ORDER BY is_day,start_at,sort_date,event_id,calendar_id LIMIT ?)")
	args = append(args, limit)
	ctes = append(ctes, "selected AS MATERIALIZED (SELECT e.event_id,e.calendar_id,CAST(e.start_at AS TEXT) AS start_key,e.start_at,e.end_at,e.start_date,e.end_date,e.payload_json FROM candidate_keys k JOIN cached_events e ON e.rowid=k.rid)")
	ctes = append(ctes, "projected AS MATERIALIZED (SELECT e.event_id,e.calendar_id,e.start_key,e.start_at,e.end_at,e.start_date,e.end_date,("+orig.Scalar+") AS scalar_ok,"+orig.Projection+" AS summary_json FROM selected e)")
	end := strings.Index(orig.Query, " SELECT CASE WHEN scalar_ok")
	if end < 0 {
		panic("missing final projection")
	}
	orig.Query = "WITH " + strings.Join(ctes, ",\n") + orig.Query[end:]
	orig.Args = args
	return orig, nil
}

type walkResult struct {
	Milliseconds float64
	Items, Pages int
	Digest       string
	Positions    []CachedPagePosition `json:"-"`
}

func walk(db *sql.DB, req CachedPageRequest, kind string) (walkResult, error) {
	var out walkResult
	var after *CachedPagePosition
	h := sha256.New()
	start := time.Now()
	for {
		b, _, err := checkedQuery(db, req, after, kind)
		if err != nil {
			return out, err
		}
		rs, err := db.Query(b.Query, b.Args...)
		if err != nil {
			return out, err
		}
		var next *CachedPagePosition
		n := 0
		for rs.Next() {
			v := make([]any, 9)
			ptr := make([]any, 9)
			for i := range v {
				ptr[i] = &v[i]
			}
			if err := rs.Scan(ptr...); err != nil {
				return out, err
			}
			if n < req.PageSize {
				pos := CachedPagePosition{EventID: fmt.Sprint(v[0]), CalendarID: fmt.Sprint(v[1]), AllDay: v[3] == nil}
				if pos.AllDay {
					pos.StartDate = fmt.Sprint(v[5])
				} else {
					pos.StartAt = fmt.Sprint(v[2])
				}
				next = &pos
				out.Positions = append(out.Positions, pos)
				blob, _ := json.Marshal([]any{v[0], v[1], v[7]})
				h.Write(blob)
				out.Items++
			}
			n++
		}
		if err := rs.Err(); err != nil {
			return out, err
		}
		rs.Close()
		out.Pages++
		if n <= req.PageSize {
			break
		}
		after = next
	}
	out.Milliseconds = float64(time.Since(start).Nanoseconds()) / 1e6
	out.Digest = fmt.Sprintf("%x", h.Sum(nil))
	return out, nil
}
func query(req CachedPageRequest, pos *CachedPagePosition, kind string) (built, error) {
	if kind == "current" {
		return current(&Store{}, req, pos)
	}
	return candidate(req, pos, strings.HasPrefix(kind, "per_source_topk"))
}
func main() {
	if (len(os.Args) < 3 || len(os.Args) > 4) || !strings.HasPrefix(os.Args[1], "/private/tmp/calendar-r6-") {
		panic("synthetic fixture path required")
	}
	outdir := os.Args[2]
	var calls int64
	sqlite.MustRegisterScalarFunction("probe_master", 1, func(_ *sqlite.FunctionContext, a []driver.Value) (driver.Value, error) { calls++; return a[0], nil })
	db, err := sql.Open("sqlite", "file:"+os.Args[1]+"?mode=ro")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ids := []string{}
	rows, err := db.Query("SELECT id FROM calendars ORDER BY id")
	if err != nil {
		panic(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	req := CachedPageRequest{ids, time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), 25}
	if len(os.Args) == 4 {
		anomalyProbe(db, req)
		return
	}
	reference, err := walk(db, req, "current")
	if err != nil {
		panic(err)
	}
	results := []map[string]any{}
	for _, kind := range []string{"current", "global_split", "per_source_topk", "global_split_guarded", "per_source_topk_guarded"} {
		samples := []float64{}
		for n := 0; n < 3; n++ {
			w, err := walk(db, req, kind)
			if err != nil {
				panic(err)
			}
			if w.Digest != reference.Digest || w.Items != reference.Items {
				panic("parity mismatch " + kind)
			}
			samples = append(samples, w.Milliseconds)
		}
		sort.Float64s(samples)
		probes := []map[string]any{}
		for _, index := range []int{-1, len(reference.Positions) / 2, len(reference.Positions) - 2} {
			var after *CachedPagePosition
			if index >= 0 {
				after = &reference.Positions[index]
			}
			b, _ := query(req, after, kind)
			q := strings.ReplaceAll(b.Query, b.Master, "probe_master("+b.Master+")")
			calls = 0
			rs, err := db.Query(q, b.Args...)
			if err != nil {
				panic(err)
			}
			n := 0
			for rs.Next() {
				n++
			}
			if err := rs.Err(); err != nil {
				panic(err)
			}
			rs.Close()
			plan, err := db.Query("EXPLAIN QUERY PLAN "+b.Query, b.Args...)
			if err != nil {
				panic(err)
			}
			details := []string{}
			for plan.Next() {
				var x, y, z int
				var d string
				plan.Scan(&x, &y, &z, &d)
				details = append(details, d)
			}
			plan.Close()
			probes = append(probes, map[string]any{"after_index": index, "master_evaluations": calls, "rows": n, "plan": details})
			if index < 0 {
				os.WriteFile(outdir+"/"+kind+".sql", []byte(b.Query), 0600)
			}
		}
		results = append(results, map[string]any{"variant": kind, "walk_ms": samples, "median_ms": samples[1], "items": reference.Items, "pages": reference.Pages, "digest": reference.Digest, "probes": probes})
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic_only": true, "runtime_changed": false, "results": results, "sources": len(ids), "scope": "SQL-only, static valid typed fixture; existing indexes forced for candidate probes; one current-reference warmup then3 sequential walks/variant; not SDK or contention measurement. Mixed/malformed/tie fixtures require separate validation."})
}

func checkedQuery(db *sql.DB, req CachedPageRequest, pos *CachedPagePosition, kind string) (built, bool, error) {
	fallback := false
	if strings.HasSuffix(kind, "_guarded") {
		q := "SELECT 1 FROM cached_events INDEXED BY cached_events_timed_range_idx WHERE calendar_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(req.CalendarIDs)), ",") + ") AND deleted=0 AND start_at IS NOT NULL AND start_at<? AND end_at>? AND typeof(start_at)<>'text' LIMIT 1"
		args := []any{}
		for _, id := range req.CalendarIDs {
			args = append(args, id)
		}
		args = append(args, req.End, req.Start)
		var n int
		err := db.QueryRow(q, args...).Scan(&n)
		if err != nil && err != sql.ErrNoRows {
			return built{}, false, err
		}
		if err == nil {
			kind = "current"
			fallback = true
		}
	}
	b, err := query(req, pos, kind)
	return b, fallback, err
}
func anomalyProbe(db *sql.DB, req CachedPageRequest) {
	var pos CachedPagePosition
	err := db.QueryRow("SELECT CAST(start_at AS TEXT),event_id,calendar_id FROM cached_events WHERE typeof(start_at)='text' ORDER BY start_at,event_id,calendar_id LIMIT 1 OFFSET 199").Scan(&pos.StartAt, &pos.EventID, &pos.CalendarID)
	if err != nil {
		panic(err)
	}
	out := []map[string]any{}
	for _, kind := range []string{"current", "global_split", "per_source_topk", "global_split_guarded", "per_source_topk_guarded"} {
		b, fallback, err := checkedQuery(db, req, &pos, kind)
		if err != nil {
			panic(err)
		}
		rs, err := db.Query(b.Query, b.Args...)
		if err != nil {
			panic(err)
		}
		bad, rows := 0, 0
		for rs.Next() {
			v := make([]any, 8)
			p := []any{}
			for i := range v {
				p = append(p, &v[i])
			}
			var invalid int
			p = append(p, &invalid)
			if err := rs.Scan(p...); err != nil {
				panic(err)
			}
			rows++
			bad += invalid
		}
		rs.Close()
		out = append(out, map[string]any{"variant": kind, "fallback": fallback, "rows": rows, "invalid_rows": bad})
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic_only": true, "results": out, "note": "Invalid numeric scalar may appear between cached_weak pages; production reader must reject a selected invalid row. Prototype native range requires fallback or equivalent handling."})
}
