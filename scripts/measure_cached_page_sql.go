//go:build ignore

// This command measures the actual Store reader on an already seeded fixture.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"calendar-mcp/internal/storage"
)

func main() {
	var database string
	flag.StringVar(&database, "database-url", "", "explicit seeded disposable SQLite fixture")
	flag.Parse()
	if !strings.HasPrefix(database, "sqlite:///private/tmp/calendar-r6-") || strings.Contains(database, "?") {
		panic("only explicit temporary calendar-r6 SQLite fixtures are allowed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := storage.Open(ctx, database)
	if err != nil {
		panic(err)
	}
	defer store.Close()
	if err := store.CheckSchema(ctx); err != nil {
		panic(err)
	}
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	req := storage.CachedPageRequest{CalendarIDs: []string{"google:r6-fixture:calendar-00"}, Start: start, End: start.Add(24 * time.Hour), PageSize: 25}
	var measurements []map[string]any
	for round := -1; round < 3; round++ {
		var after *storage.CachedPagePosition
		var sqlNS int64
		pages, items := 0, 0
		started := time.Now()
		for {
			page, err := store.ReadCachedPage(ctx, req, after)
			if err != nil {
				panic(err)
			}
			pages++
			sqlNS += page.Stats.SQLEventsNS
			n := min(len(page.Rows), req.PageSize)
			items += n
			if len(page.Rows) <= req.PageSize {
				break
			}
			position := page.Rows[n-1].Position
			after = &position
		}
		if items != 4000 || pages != 160 {
			panic(errors.New("expected the explicitly seeded 4000-event fixture"))
		}
		if round >= 0 {
			measurements = append(measurements, map[string]any{"round": round, "items": items, "pages": pages,
				"sql_events_ms": float64(sqlNS) / 1e6, "store_wall_ms": float64(time.Since(started).Nanoseconds()) / 1e6})
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic_only": true, "provider_calls": 0, "pool_tuning": false,
		"scope": "actual Store.ReadCachedPage; SQL events timing excludes metadata, JSON decoding, authority, Pack and HTTP; one warmup then three walks", "measurements": measurements}); err != nil {
		panic(fmt.Errorf("write measurements: %w", err))
	}
}
