//go:build ignore

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"os"
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

func build(s *Store, req CachedPageRequest, after *CachedPagePosition) (built, error) {
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
	query := fmt.Sprintf(`WITH selected AS MATERIALIZED (
SELECT e.event_id,e.calendar_id,%s AS start_key,e.start_at,e.end_at,e.start_date,e.end_date,
(%s) AS scalar_ok,%s AS summary_json
FROM cached_events e JOIN calendars c ON c.id=e.calendar_id JOIN connections conn ON conn.id=c.connection_id
WHERE e.calendar_id IN (%s) AND c.can_read=? AND conn.status=? AND e.deleted=? AND %s AND COALESCE(%s,'')<>'seriesMaster'`, startKey, scalarOK, projection, placeholders, overlap, master)
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
	query += ` ORDER BY e.start_at IS NULL,e.start_at,CASE WHEN e.start_at IS NULL THEN e.start_date END,e.event_id,e.calendar_id LIMIT ?)`
	safe := func(expression string) string { return "CASE WHEN scalar_ok THEN " + expression + " ELSE NULL END" }
	query += fmt.Sprintf(` SELECT %s,%s,%s,%s,%s,%s,%s,CASE WHEN scalar_ok AND %s<=%d THEN summary_json ELSE NULL END,CASE WHEN scalar_ok THEN 0 ELSE 1 END
FROM selected ORDER BY start_at IS NULL,start_at,CASE WHEN start_at IS NULL THEN start_date END,event_id,calendar_id`, safe("event_id"), safe("calendar_id"), safe("start_key"), safe("start_at"), safe("end_at"), safe("start_date"), safe("end_date"), summarySize, cachedPageMaxPayloadBytes)
	args = append(args, req.PageSize+1)
	return built{query, args, projection, scalarOK, master}, nil
}
func variant(b built, name string) string {
	q := b.Query
	if strings.Contains(name, "cursor-first") {
		clause := " AND COALESCE(" + b.Master + ",'')<>'seriesMaster'"
		q = strings.Replace(q, clause, "", 1)
		q = strings.Replace(q, " ORDER BY e.start_at IS NULL", clause+" ORDER BY e.start_at IS NULL", 1)
	}
	if strings.Contains(name, "late-project") {
		old := "(" + b.Scalar + ") AS scalar_ok," + b.Projection + " AS summary_json"
		if !strings.Contains(q, old) {
			panic("missing projection")
		}
		q = strings.Replace(q, old, "e.payload_json AS payload_json", 1)
		boundary := ") SELECT "
		i := strings.Index(q, boundary)
		if i < 0 {
			panic("missing CTE boundary")
		}
		q = q[:i+1] + ", projected AS MATERIALIZED (SELECT e.*, (" + b.Scalar + ") AS scalar_ok," + b.Projection + " AS summary_json FROM selected e)" + q[i+1:]
		q = strings.Replace(q, "FROM selected ORDER BY", "FROM projected ORDER BY", 1)
	}
	return q
}

type result struct {
	Name                                             string
	WalkMS                                           []float64
	MedianMS                                         float64
	Pages, Rows                                      int
	JSONMasterEvaluations, JSONProjectionEvaluations int64
	Plan                                             []string
}

func pg(q string) string {
	n := 0
	var b strings.Builder
	for _, c := range q {
		if c == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}
func main() {
	if len(os.Args) != 3 || !strings.HasPrefix(os.Args[1], "postgres://calendar_r6@127.0.0.1:55467/calendar_r6_baseline?") || !strings.Contains(os.Args[1], "search_path=r7_diag_20261117") {
		panic("synthetic PG fixture only")
	}
	db, e := sql.Open("pgx", os.Args[1])
	if e != nil {
		panic(e)
	}
	defer db.Close()
	rows, e := db.Query("SELECT cal.id,c.id,c.authorization_revision,cal.detail_receipt FROM calendars cal JOIN connections c ON c.id=cal.connection_id ORDER BY cal.id")
	if e != nil {
		panic(e)
	}
	ids := []string{}
	tickets := [][]any{}
	for rows.Next() {
		var id, c, rev, receipt string
		rows.Scan(&id, &c, &rev, &receipt)
		ids = append(ids, id)
		tickets = append(tickets, []any{c, id, rev, receipt, true})
	}
	rows.Close()
	req := CachedPageRequest{ids, time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), 25}
	out := []map[string]any{}
	var a time.Time
	var eventID, calID string
	db.QueryRow("SELECT start_at,event_id,calendar_id FROM cached_events ORDER BY start_at,event_id,calendar_id OFFSET 2000 LIMIT 1").Scan(&a, &eventID, &calID)
	for _, position := range []string{"first", "middle"} {
		var after *CachedPagePosition
		if position == "middle" {
			after = &CachedPagePosition{StartAt: a.Format(time.RFC3339Nano), EventID: eventID, CalendarID: calID}
		}
		b, e := build(&Store{DialectPostgres}, req, after)
		if e != nil {
			panic(e)
		}
		for _, name := range []string{"current", "late-project"} {
			q := pg(variant(b, name))
			times := []float64{}
			for i := 0; i < 11; i++ {
				start := time.Now()
				rs, e := db.Query(q, b.Args...)
				if e != nil {
					panic(e)
				}
				for rs.Next() {
					v := make([]any, 9)
					p := make([]any, 9)
					for j := range v {
						p[j] = &v[j]
					}
					if e := rs.Scan(p...); e != nil {
						panic(e)
					}
				}
				if e := rs.Err(); e != nil {
					panic(e)
				}
				rs.Close()
				if i > 0 {
					times = append(times, float64(time.Since(start).Nanoseconds())/1e6)
				}
			}
			var plan []byte
			e = db.QueryRow("EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+q, b.Args...).Scan(&plan)
			if e != nil {
				panic(e)
			}
			os.WriteFile(os.Args[2]+"/pg-"+position+"-"+name+"-explain.json", plan, 0600)
			out = append(out, map[string]any{"position": position, "variant": name, "query_ms": times})
		}
	}
	clause := `SELECT COUNT(*) FROM connections c JOIN calendars cal ON cal.connection_id=c.id JOIN calendar_sync_state st ON st.calendar_id=cal.id WHERE c.id=? AND cal.id=? AND c.status='connected' AND c.authorization_revision=? AND cal.detail_receipt=? AND cal.can_read=? AND (st.last_error_code IS NULL OR st.last_error_code NOT IN ('auth','permission'))`
	vals := []string{}
	args := []any{}
	for _, t := range tickets {
		vals = append(vals, "(?,?,?,?)")
		args = append(args, t[:4]...)
	}
	batched := `WITH expected(connection_id,calendar_id,rev,receipt) AS (VALUES ` + strings.Join(vals, ",") + `) SELECT COUNT(*) FROM expected x JOIN connections c ON c.id=x.connection_id JOIN calendars cal ON cal.id=x.calendar_id AND cal.connection_id=c.id JOIN calendar_sync_state st ON st.calendar_id=cal.id WHERE c.status='connected' AND c.authorization_revision=x.rev AND cal.detail_receipt=x.receipt AND cal.can_read=true AND (st.last_error_code IS NULL OR st.last_error_code NOT IN ('auth','permission'))`
	auth := map[string][]float64{}
	for round := 0; round < 21; round++ {
		for _, kind := range []string{"sixteen_queries", "single_query"} {
			start := time.Now()
			tx, e := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
			if e != nil {
				panic(e)
			}
			if kind == "sixteen_queries" {
				for _, t := range tickets {
					var n int
					if e := tx.QueryRow(pg(clause), t...).Scan(&n); e != nil || n != 1 {
						panic(fmt.Sprint(e, n))
					}
				}
			} else {
				var n int
				if e := tx.QueryRow(pg(batched), args...).Scan(&n); e != nil || n != 16 {
					panic(fmt.Sprint(e, n))
				}
			}
			if e := tx.Commit(); e != nil {
				panic(e)
			}
			if round > 0 {
				auth[kind] = append(auth[kind], float64(time.Since(start).Nanoseconds())/1e6)
			}
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic_only": true, "sources": 16, "events": 4000, "query_experiment": out, "authority_experiment_ms": auth, "note": "SQL microbenchmarks, no runtime implementation; warmups excluded; authority includes begin/commit; valid grants only, no negative contract validation"})
}
