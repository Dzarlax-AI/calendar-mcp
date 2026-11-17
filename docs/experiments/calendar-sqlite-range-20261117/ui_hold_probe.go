//go:build ignore

package main

import (
	"calendar-mcp/internal/calendar"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 2 || !strings.HasPrefix(os.Args[1], "/private/tmp/calendar-r6-") {
		panic("explicit synthetic fixture required")
	}
	db, err := sql.Open("sqlite", "file:"+os.Args[1]+"?mode=ro")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ids := []string{}
	rs, err := db.Query("SELECT id FROM calendars ORDER BY id")
	if err != nil {
		panic(err)
	}
	for rs.Next() {
		var id string
		rs.Scan(&id)
		ids = append(ids, id)
	}
	rs.Close()
	q := `SELECT e.payload_json FROM cached_events e JOIN calendars c ON c.id=e.calendar_id JOIN connections conn ON conn.id=c.connection_id WHERE e.calendar_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `) AND c.can_read=? AND conn.status=? AND e.deleted=? AND ((start_at IS NOT NULL AND start_at<? AND end_at>?) OR (start_date IS NOT NULL AND start_date<? AND end_date>?)) ORDER BY e.start_at IS NULL,e.start_at,e.start_date,e.event_id`
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	args := []any{}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, true, "connected", false, start.Add(time.Hour), start, "2026-11-18", "2026-11-17")
	results := []map[string]any{}
	reference := ""
	for _, kind := range []string{"decode_with_open_rows", "bounded_decode_after_close"} {
		holds, totals := []float64{}, []float64{}
		fallbacks := 0
		var maxBuffer int
		for round := -1; round < 20; round++ {
			started := time.Now()
			rows, err := db.Query(q, args...)
			if err != nil {
				panic(err)
			}
			events := []calendar.EventV2{}
			raws := [][]byte{}
			buffering := kind == "bounded_decode_after_close"
			size := 0
			decode := func(raw []byte) {
				var e calendar.EventV2
				if err := json.Unmarshal(raw, &e); err != nil {
					panic(err)
				}
				if e.InstanceKind != "seriesMaster" {
					events = append(events, e)
				}
			}
			for rows.Next() {
				var raw []byte
				if err := rows.Scan(&raw); err != nil {
					panic(err)
				}
				if buffering && size+len(raw) <= 8<<20 {
					raws = append(raws, raw)
					size += len(raw)
					if size > maxBuffer {
						maxBuffer = size
					}
					continue
				}
				if buffering {
					for i, v := range raws {
						decode(v)
						raws[i] = nil
					}
					raws = nil
					buffering = false
					if round >= 0 {
						fallbacks++
					}
				}
				decode(raw)
			}
			if err := rows.Err(); err != nil {
				panic(err)
			}
			rows.Close()
			held := time.Since(started)
			for i, raw := range raws {
				decode(raw)
				raws[i] = nil
			}
			total := time.Since(started)
			payload, _ := json.Marshal(events)
			digest := fmt.Sprintf("%x", sha256.Sum256(payload))
			if reference == "" {
				reference = digest
			}
			if digest != reference {
				panic("parity mismatch")
			}
			if len(events) != 3599 {
				panic(fmt.Sprintf("unexpected count %d", len(events)))
			}
			if round >= 0 {
				holds = append(holds, float64(held.Nanoseconds())/1e6)
				totals = append(totals, float64(total.Nanoseconds())/1e6)
			}
		}
		sort.Float64s(holds)
		sort.Float64s(totals)
		results = append(results, map[string]any{"variant": kind, "connection_hold_ms": holds, "total_ms": totals, "median_hold_ms": holds[len(holds)/2], "median_total_ms": totals[len(totals)/2], "fallbacks": fallbacks, "max_raw_buffer_bytes": maxBuffer, "raw_buffer_cap_bytes": 8 << 20, "digest": reference})
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic_only": true, "runtime_changed": false, "rounds": 20, "results": results, "scope": "isolated copy of the current background full-event SELECT/JSON decode; source-status SQL not included; full event parity; bounded per-call raw buffer, no reusable cache; no end-to-end candidate measurement"})
}
