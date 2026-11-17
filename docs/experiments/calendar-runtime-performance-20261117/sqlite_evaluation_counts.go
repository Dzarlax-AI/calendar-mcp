//go:build ignore

package main

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	sqlite "modernc.org/sqlite"
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

func main() {
	counters := map[string]int64{}
	sqlite.MustRegisterScalarFunction("diag_count", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		counters[args[0].(string)]++
		return int64(1), nil
	})
	db, e := sql.Open("sqlite", "file:/private/tmp/calendar-r6-diagnosis-20261117.db?mode=ro")
	if e != nil {
		panic(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var id string
	db.QueryRow("SELECT id FROM calendars LIMIT 1").Scan(&id)
	req := CachedPageRequest{[]string{id}, time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), 25}
	out := []map[string]any{}
	for _, offset := range []int{0, 1999, 3974} {
		var after *CachedPagePosition
		if offset > 0 {
			after = &CachedPagePosition{}
			db.QueryRow("SELECT CAST(start_at AS TEXT),event_id,calendar_id FROM cached_events ORDER BY start_at,event_id,calendar_id LIMIT 1 OFFSET ?", offset).Scan(&after.StartAt, &after.EventID, &after.CalendarID)
		}
		b, e := build(&Store{}, req, after)
		if e != nil {
			panic(e)
		}
		for _, name := range []string{"current", "cursor-first", "late-project", "cursor-first+late-project"} {
			q := variant(b, name)
			q = strings.ReplaceAll(q, b.Master, "CASE WHEN diag_count('master') THEN "+b.Master+" END")
			q = strings.ReplaceAll(q, b.Projection, "CASE WHEN diag_count('projection') THEN "+b.Projection+" END")
			counters["master"] = 0
			counters["projection"] = 0
			rs, e := db.Query(q, b.Args...)
			if e != nil {
				panic(e)
			}
			for rs.Next() {
			}
			if e := rs.Err(); e != nil {
				panic(e)
			}
			rs.Close()
			out = append(out, map[string]any{"after_offset": offset, "variant": name, "master_evaluations": counters["master"], "projection_evaluations": counters["projection"]})
		}
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
