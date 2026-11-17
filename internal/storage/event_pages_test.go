package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"

	"modernc.org/sqlite"
)

func newCachedPagesStore(t *testing.T, calendarSpecs ...[3]string) (*Store, time.Time, []string) {
	t.Helper()
	store := newSQLiteStore(t)
	now := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	connectionIDs := map[string]bool{}
	ids := make([]string, 0, len(calendarSpecs))
	for _, spec := range calendarSpecs {
		connectionID, rawID, provider := spec[0], spec[1], spec[2]
		if !connectionIDs[connectionID] {
			if err := store.CreateConnection(ctx, Connection{ID: connectionID, Provider: "google", DisplayName: "Synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			connectionIDs[connectionID] = true
		}
		calendarID := "google:" + connectionID + ":" + rawID
		ids = append(ids, calendarID)
		if err := store.UpsertCalendar(ctx, Calendar{ID: calendarID, ConnectionID: connectionID, ProviderCalendarID: rawID, Name: "Synthetic", CanRead: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
		if provider != "google" {
			t.Fatalf("test helper provider must be google, got %q", provider)
		}
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.AddDate(-1, 0, 0), End: now.AddDate(2, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	for connectionID := range connectionIDs {
		snapshot, err := store.BeginAuthorization(ctx, connectionID, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		staged := []DiscoveredDetailCalendar{}
		for _, spec := range calendarSpecs {
			if spec[0] == connectionID {
				calendarID := "google:" + spec[0] + ":" + spec[1]
				staged = append(staged, DiscoveredDetailCalendar{Calendar: Calendar{ID: calendarID, ConnectionID: connectionID, ProviderCalendarID: spec[1], Name: "Synthetic"}, AccessRole: "reader"})
			}
		}
		if err := store.CompleteGoogleAuthorization(ctx, snapshot, staged, now); err != nil {
			t.Fatal(err)
		}
	}
	return store, now, ids
}

func pageEvent(id, calendarID, start, end string, allDay bool) calendar.EventV2 {
	event := calendar.EventV2{ID: id, CalendarID: calendarID, Provider: "google", Title: id, Description: strings.Repeat("secret description ", 3000)}
	if allDay {
		event.Start, event.End = calendar.EventTime{Date: start}, calendar.EventTime{Date: end}
	} else {
		event.Start, event.End = calendar.EventTime{DateTime: start, TimeZone: "UTC"}, calendar.EventTime{DateTime: end, TimeZone: "UTC"}
	}
	return event
}

func TestReadCachedPageMixedOrderingStrayDateAndMasterFiltering(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"}, [3]string{"conn", "shared:team", "google"})
	ctx := context.Background()
	start := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	for _, item := range []struct {
		id, cal, start, end string
		allDay              bool
	}{
		{"timed-a", ids[0], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false},
		{"timed-b", ids[1], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false},
		{"day-a", ids[0], "2026-08-22", "2026-08-23", true},
		{"day-b", ids[1], "2026-08-22", "2026-08-23", true},
		{"master", ids[0], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false},
	} {
		event := pageEvent(item.id, item.cal, item.start, item.end, item.allDay)
		if item.id == "master" {
			event.InstanceKind = "seriesMaster"
		}
		if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE cached_events SET start_date='2026-08-22' WHERE event_id IN ('timed-a','timed-b')`); err != nil {
		t.Fatal(err)
	}
	req := CachedPageRequest{CalendarIDs: ids, Start: start, End: end, PageSize: 5}
	page, err := store.ReadCachedPage(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"timed-a", "timed-b", "day-a", "day-b"}
	if len(page.Rows) != len(want) {
		t.Fatalf("rows=%#v want %v", page.Rows, want)
	}
	for i, row := range page.Rows {
		if row.Summary.EventID != want[i] {
			t.Fatalf("row %d id=%q want %q", i, row.Summary.EventID, want[i])
		}
	}
	if page.Rows[0].Position.StartDate != "" || page.Rows[2].Position.StartDate != "2026-08-22" {
		t.Fatalf("timed/all-day key shape is wrong: %#v %#v", page.Rows[0].Position, page.Rows[2].Position)
	}
	if len(page.Tickets) != len(ids) || page.Stats.RowsDecoded != len(want) || page.Stats.PayloadBytes > 4000 {
		t.Fatalf("ticket/stat mismatch: tickets=%d stats=%+v", len(page.Tickets), page.Stats)
	}
	// The master sorts before both timed events and must not consume the
	// bounded page or lookahead slot.
	req.PageSize = 1
	first, err := store.ReadCachedPage(ctx, req, nil)
	if err != nil || len(first.Rows) != 2 || first.Rows[0].Summary.EventID != "timed-a" || first.Rows[1].Summary.EventID != "timed-b" {
		t.Fatalf("master consumed page limit: rows=%#v err=%v", first.Rows, err)
	}
	second, err := store.ReadCachedPage(ctx, req, &first.Rows[0].Position)
	if err != nil || len(second.Rows) != 2 || second.Rows[0].Summary.EventID != "timed-b" || second.Rows[1].Summary.EventID != "day-a" {
		t.Fatalf("continuation across timed/all-day boundary: rows=%#v err=%v", second.Rows, err)
	}
}

func TestReadCachedPageSQLiteMasterFilterWithEscapedJSON(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"})
	ctx := t.Context()
	for _, item := range []struct {
		id, description, escapedField string
		master, allDay                bool
	}{
		{id: "a-master", master: true},
		{id: "b-escaped-value", master: true, escapedField: `"instance_kind":"series\u004daster"`},
		{id: "c-description-match", description: "seriesMaster"},
		{id: "d-normal"},
		{id: "e-escaped-key", master: true, allDay: true, escapedField: `"instance_\u006bind":"s\u0065riesMaster"`},
		{id: "f-all-day", allDay: true},
		{id: "g-all-day", allDay: true},
	} {
		event := pageEvent(item.id, ids[0], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false)
		if item.allDay {
			event = pageEvent(item.id, ids[0], "2026-08-22", "2026-08-23", true)
		}
		event.Description = item.description
		if item.master {
			event.InstanceKind = "seriesMaster"
		}
		if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
		if item.escapedField != "" {
			_, err := store.db.ExecContext(ctx, `UPDATE cached_events SET payload_json=replace(payload_json,?,?) WHERE calendar_id=? AND event_id=?`,
				`"instance_kind":"seriesMaster"`, item.escapedField, ids[0], item.id)
			if err != nil {
				t.Fatal(err)
			}
			var escaped bool
			if err := store.db.QueryRowContext(ctx, `SELECT instr(payload_json,?)>0 FROM cached_events WHERE calendar_id=? AND event_id=?`, item.escapedField, ids[0], item.id).Scan(&escaped); err != nil || !escaped {
				t.Fatalf("escaped fixture %s was not stored: %v", item.id, err)
			}
		}
	}
	req := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(4 * time.Hour), PageSize: 1}
	var after *CachedPagePosition
	want := []string{"c-description-match", "d-normal", "f-all-day", "g-all-day"}
	for i, id := range want {
		page, err := store.ReadCachedPage(ctx, req, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) != min(2, len(want)-i) || page.Rows[0].Summary.EventID != id || (len(page.Rows) == 2 && page.Rows[1].Summary.EventID != want[i+1]) {
			t.Fatalf("cursor page for %s: %+v", id, page.Rows)
		}
		after = &page.Rows[0].Position
	}
}

func TestReadCachedPageNormalizesUTCAndHonorsStrictFractionalOverlap(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"}, [3]string{"conn", "shared:team", "google"})
	ctx := context.Background()
	timed := func(id, calendarID, start, end string) calendar.EventV2 {
		return calendar.EventV2{ID: id, CalendarID: calendarID, Provider: "google", Title: id,
			Start: calendar.EventTime{DateTime: start, TimeZone: "Europe/Belgrade"},
			End:   calendar.EventTime{DateTime: end, TimeZone: "Europe/Belgrade"}}
	}
	for _, event := range []calendar.EventV2{
		timed("starts-at-lower", ids[0], "2026-08-22T11:30:00.250000000+02:00", "2026-08-22T11:31:00.000000000+02:00"),
		timed("same-key", ids[0], "2026-08-22T12:00:00.123456789+02:00", "2026-08-22T12:15:00.000000000+02:00"),
		timed("same-key", ids[1], "2026-08-22T12:00:00.123456789+02:00", "2026-08-22T12:15:00.000000000+02:00"),
		timed("before-upper", ids[0], "2026-08-22T12:30:00.499999999+02:00", "2026-08-22T12:45:00.000000000+02:00"),
		timed("starts-at-upper", ids[0], "2026-08-22T12:30:00.500000000+02:00", "2026-08-22T12:45:00.000000000+02:00"),
		timed("ends-at-lower", ids[0], "2026-08-22T11:00:00.000000000+02:00", "2026-08-22T11:30:00.250000000+02:00"),
		timed("outside-stray-date", ids[0], "2026-08-22T13:00:00.000000000+02:00", "2026-08-22T14:00:00.000000000+02:00"),
	} {
		if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
	}
	// Timed rows can carry stale/stray date columns; those columns must never
	// make a timed row eligible for the all-day date scan.
	if _, err := store.db.ExecContext(ctx, `UPDATE cached_events SET start_date='2026-08-22', end_date='2026-08-23' WHERE event_id='outside-stray-date'`); err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse(time.RFC3339Nano, "2026-08-22T11:30:00.250000000+02:00")
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse(time.RFC3339Nano, "2026-08-22T12:30:00.500000000+02:00")
	if err != nil {
		t.Fatal(err)
	}
	request := CachedPageRequest{CalendarIDs: ids, Start: start, End: end, PageSize: 10}
	if _, err := store.CaptureCachedPageAuthority(ctx, request); err != nil {
		t.Fatalf("non-UTC authority range rejected: %v", err)
	}
	page, err := store.ReadCachedPage(ctx, request, nil)
	if err != nil {
		t.Fatalf("non-UTC reader range rejected: %v", err)
	}
	want := []string{"starts-at-lower", "same-key", "same-key", "before-upper"}
	got := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		got = append(got, row.Summary.EventID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("offset/fractional strict overlap mismatch: got=%v want=%v", got, want)
	}
	if page.Rows[1].Position.CalendarID != ids[0] || page.Rows[2].Position.CalendarID != ids[1] {
		t.Fatalf("equal timestamp calendar tie order mismatch: %#v %#v", page.Rows[1].Position, page.Rows[2].Position)
	}
	if page.Rows[1].Summary.Start.TimeZone != "Europe/Belgrade" {
		t.Fatalf("UTC scalar normalization changed the payload timezone: %+v", page.Rows[1].Summary.Start)
	}
}

func TestReadCachedPageUsesLookaheadAndContinuesWithoutDuplicates(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("event-%d", i)
		start := time.Date(2026, 8, 22, 10, i, 0, 0, time.UTC)
		if err := store.UpsertCachedEvent(ctx, pageEvent(id, ids[0], start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339), false), now); err != nil {
			t.Fatal(err)
		}
	}
	req := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(24 * time.Hour), PageSize: 2}
	var after *CachedPagePosition
	var got []string
	for {
		page, err := store.ReadCachedPage(ctx, req, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) > req.PageSize+1 {
			t.Fatalf("read returned %d rows", len(page.Rows))
		}
		returned := page.Rows
		if len(returned) > req.PageSize {
			returned = returned[:req.PageSize]
		}
		for _, row := range returned {
			got = append(got, row.Summary.EventID)
		}
		if len(page.Rows) <= req.PageSize {
			break
		}
		last := returned[len(returned)-1].Position
		after = &last
	}
	if want := []string{"event-0", "event-1", "event-2", "event-3", "event-4"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("continuation skipped or duplicated events: got %v want %v", got, want)
	}
}

func TestReadCachedPageRejectsMissingOrPartialAuthorityAndWindow(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"})
	ctx := context.Background()
	req := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(time.Hour), PageSize: 10}
	if _, err := store.CaptureCachedPageAuthority(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureCachedPageAuthority(ctx, CachedPageRequest{CalendarIDs: []string{ids[0], "google:missing:unknown"}, Start: req.Start, End: req.End, PageSize: req.PageSize}); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("partial source scope error=%v", err)
	}
	if _, err := store.CaptureCachedPageAuthority(ctx, CachedPageRequest{CalendarIDs: ids, Start: now.AddDate(-5, 0, 0), End: req.End, PageSize: req.PageSize}); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("out-of-window error=%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM calendar_sync_state WHERE calendar_id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureCachedPageAuthority(ctx, req); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("missing sync state error=%v", err)
	}
}

func TestReadCachedPageWideScopeAndKnownPermissionFailure(t *testing.T) {
	specs := make([][3]string, 16)
	for i := range specs {
		specs[i] = [3]string{"wide", fmt.Sprintf("calendar-%02d", i), "google"}
	}
	store, now, ids := newCachedPagesStore(t, specs...)
	req := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(time.Hour), PageSize: 25}
	page, err := store.ReadCachedPage(context.Background(), req, nil)
	if err != nil || len(page.Sources) != 16 || len(page.Tickets) != 16 {
		t.Fatalf("16-calendar scope failed: sources=%d tickets=%d err=%v", len(page.Sources), len(page.Tickets), err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE calendar_sync_state SET last_error_code='permission' WHERE calendar_id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureCachedPageAuthority(context.Background(), req); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("known permission failure should close whole scope: %v", err)
	}
}

func TestReadCachedPageBoundsProjectedSummaryAndStoredIDs(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"})
	ctx := context.Background()
	event := pageEvent("jumbo", ids[0], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false)
	event.Title = strings.Repeat("x", 40000)
	if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
		t.Fatal(err)
	}
	req := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(24 * time.Hour), PageSize: 10}
	page, err := store.ReadCachedPage(ctx, req, nil)
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("large summary page rows=%d err=%v", len(page.Rows), err)
	}
	marker := page.Rows[0].Summary
	if marker.Kind != "oversized" || marker.EventID != "jumbo" || marker.CalendarID != ids[0] || marker.Start.DateTime == "" || marker.Reason != "summary_projection_too_large" {
		t.Fatalf("oversized row marker lost references/range: %#v", marker)
	}
	longID := strings.Repeat("i", cachedPageMaxIDBytes+1)
	if _, err := store.db.ExecContext(ctx, `UPDATE cached_events SET event_id=? WHERE event_id='jumbo'`, longID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCachedPage(ctx, req, nil); !errors.Is(err, ErrInvalidCachedPageData) {
		t.Fatalf("oversized stored event ID should fail before projection: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE cached_events SET event_id='jumbo', payload_json='not json' WHERE event_id=?`, longID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCachedPage(ctx, req, nil); !errors.Is(err, ErrInvalidCachedPageData) {
		t.Fatalf("malformed cached payload should fail as bounded data error: %v", err)
	}
}

func TestReadCachedPageValidatesRequestAndPosition(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"})
	base := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(time.Hour), PageSize: 1}
	for _, req := range []CachedPageRequest{
		{CalendarIDs: ids, Start: now, End: now, PageSize: 1},
		{CalendarIDs: append(append([]string{}, ids...), ids[0]), Start: now, End: now.Add(time.Hour), PageSize: 1},
		{CalendarIDs: []string{"microsoft:conn:calendar"}, Start: now, End: now.Add(time.Hour), PageSize: 1},
		{CalendarIDs: ids, Start: now, End: now.Add(time.Hour), PageSize: 51},
	} {
		if _, err := store.ReadCachedPage(context.Background(), req, nil); err == nil {
			t.Fatalf("accepted invalid request: %#v", req)
		}
	}
	if _, err := store.ReadCachedPage(context.Background(), base, &CachedPagePosition{StartAt: "2026-08-22T10:00:00Z", EventID: "other", CalendarID: "google:elsewhere:calendar"}); err == nil {
		t.Fatal("accepted cursor outside requested calendars")
	}
}

func TestCachedPageSQLiteRangeArmsStayCovering(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"})
	dateEnd := now.Add(24 * time.Hour).Format(calendar.DateLayout)
	dateStart := now.UTC().Format(calendar.DateLayout)
	cases := []struct {
		name, index, query string
		args               []any
	}{
		{
			name:  "timed",
			index: "cached_events_timed_range_idx",
			query: `SELECT rowid FROM cached_events WHERE calendar_id IN (?) AND deleted=? AND start_at IS NOT NULL AND start_at<? AND end_at>?`,
			args:  []any{ids[0], false, now.Add(2 * time.Hour), now},
		},
		{
			name:  "date",
			index: "cached_events_date_range_idx",
			query: `SELECT rowid FROM cached_events WHERE calendar_id IN (?) AND deleted=? AND start_date IS NOT NULL AND start_date<? AND end_date>?`,
			args:  []any{ids[0], false, dateEnd, dateStart},
		},
	}
	var tableRootPage int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT rootpage FROM sqlite_master WHERE type='table' AND name='cached_events'`).Scan(&tableRootPage); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := store.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			foundCoveringIndex := false
			for plan.Next() {
				var id, parent, unused int
				var detail string
				if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Logf("EQP %s: %s", tc.name, detail)
				if strings.Contains(detail, "USING COVERING INDEX "+tc.index) {
					foundCoveringIndex = true
				}
			}
			if err := plan.Err(); err != nil {
				t.Fatal(err)
			}
			if err := plan.Close(); err != nil {
				t.Fatal(err)
			}
			if !foundCoveringIndex {
				t.Fatalf("query plan did not use expected covering index %s", tc.index)
			}

			program, err := store.db.QueryContext(context.Background(), "EXPLAIN "+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			for program.Next() {
				var address, p1, p2, p3, p5 int
				var opcode, p4, comment sql.NullString
				if err := program.Scan(&address, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
					t.Fatal(err)
				}
				t.Logf("VDBE %s: addr=%d op=%s p1=%d p2=%d p3=%d p4=%s", tc.name, address, opcode.String, p1, p2, p3, p4.String)
				if opcode.String == "OpenRead" && int64(p2) == tableRootPage {
					t.Fatalf("VDBE opens the cached_events table cursor; expected index-only rowid scan (addr=%d cursor=%d root=%d index=%s)", address, p1, p2, tc.index)
				}
			}
			if err := program.Err(); err != nil {
				t.Fatal(err)
			}
			if err := program.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCachedPageSQLiteProjectsOnlySelectedRows(t *testing.T) {
	var calls atomic.Int64
	fn := fmt.Sprintf("test_cached_page_projection_count_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(fn, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		calls.Add(1)
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "primary", "google"})
	ctx := context.Background()
	for i := 0; i < 400; i++ {
		event := pageEvent(fmt.Sprintf("event-%03d", i), ids[0], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false)
		event.Description = ""
		if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
	}
	req := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(24 * time.Hour), PageSize: 25}
	initial, err := store.ReadCachedPage(ctx, req, nil)
	if err != nil || len(initial.Rows) != 26 {
		t.Fatalf("initial page: %d %v", len(initial.Rows), err)
	}
	middle, last := initial.Rows[0].Position, initial.Rows[0].Position
	middle.EventID, last.EventID = "event-199", "event-398"
	for _, tc := range []struct {
		name  string
		after *CachedPagePosition
	}{{"first", nil}, {"middle", &middle}, {"last", &last}} {
		t.Run(tc.name, func(t *testing.T) {
			query, args, err := cachedPageEventsQuery(store, req, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				t.Fatal(err)
			}
			var details []string
			for plan.Next() {
				var a, b, c int
				var detail string
				if err := plan.Scan(&a, &b, &c, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := plan.Err(); err != nil {
				t.Fatal(err)
			}
			if err := plan.Close(); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(details, "\n")
			for _, required := range []string{"MATERIALIZE selected", "MATERIALIZE projected", "USING COVERING INDEX cached_events_timed_range_idx", "USING COVERING INDEX cached_events_date_range_idx"} {
				if !strings.Contains(joined, required) {
					t.Fatalf("missing %q in actual query plan:\n%s", required, joined)
				}
			}
			// Wrap the complete summary expression of the production-generated
			// query, preserving all other SQL and parameters.
			const begin, end = " THEN json_object(", " ELSE NULL END AS summary_json"
			start := strings.Index(query, begin)
			finish := strings.Index(query, end)
			if start < 0 || finish < start || strings.Count(query, begin) != 1 || strings.Count(query, end) != 1 {
				t.Fatal("summary projection shape changed")
			}
			start += len(" THEN ")
			instrumented := query[:start] + fn + "(" + query[start:finish] + ")" + query[finish:]
			calls.Store(0)
			rows, err := store.db.QueryContext(ctx, instrumented, args...)
			if err != nil {
				t.Fatal(err)
			}
			var gotIDs, gotTitles []string
			for rows.Next() {
				var id, cal, key, startAt, endAt, startDate, endDate, raw any
				var invalid int
				if err := rows.Scan(&id, &cal, &key, &startAt, &endAt, &startDate, &endDate, &raw, &invalid); err != nil {
					t.Fatal(err)
				}
				if invalid != 0 {
					t.Fatal("invalid row")
				}
				var summary struct {
					Title string `json:"title"`
				}
				var payload []byte
				switch v := raw.(type) {
				case string:
					payload = []byte(v)
				case []byte:
					payload = v
				default:
					t.Fatalf("summary type %T", raw)
				}
				if err := json.Unmarshal(payload, &summary); err != nil {
					t.Fatal(err)
				}
				gotIDs = append(gotIDs, fmt.Sprint(id))
				gotTitles = append(gotTitles, summary.Title)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if n := calls.Load(); n != int64(len(gotIDs)) || n > int64(req.PageSize+1) {
				t.Fatalf("projections=%d rows=%d limit=%d", n, len(gotIDs), req.PageSize+1)
			}
			page, err := store.ReadCachedPage(ctx, req, tc.after)
			if err != nil || len(page.Rows) != len(gotIDs) {
				t.Fatalf("production page count=%d instrumented=%d err=%v", len(page.Rows), len(gotIDs), err)
			}
			for i, row := range page.Rows {
				if row.Summary.EventID != gotIDs[i] || row.Summary.Title != gotTitles[i] {
					t.Fatalf("row %d differs: %+v / %s %s", i, row.Summary, gotIDs[i], gotTitles[i])
				}
			}
		})
	}
}

func TestCachedPageSQLiteSplitCursorAndMalformedFallback(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"conn", "a", "google"}, [3]string{"conn", "b", "google"})
	ctx := context.Background()
	for _, ev := range []calendar.EventV2{
		pageEvent("same", ids[0], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false),
		pageEvent("same", ids[1], "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z", false),
		pageEvent("day", ids[0], "2026-08-22", "2026-08-23", true),
	} {
		if err := store.UpsertCachedEvent(ctx, ev, now); err != nil {
			t.Fatal(err)
		}
	}
	req := CachedPageRequest{CalendarIDs: ids, Start: now, End: now.Add(2 * time.Hour), PageSize: 1}
	first, err := store.ReadCachedPage(ctx, req, nil)
	if err != nil || len(first.Rows) != 2 {
		t.Fatalf("first: %v %#v", err, first.Rows)
	}
	second, err := store.ReadCachedPage(ctx, req, &first.Rows[0].Position)
	if err != nil || len(second.Rows) != 2 || second.Rows[0].Position.CalendarID != ids[1] || second.Rows[1].Summary.EventID != "day" {
		t.Fatalf("cursor/ceil: %v %#v", err, second.Rows)
	}
	q, args, err := cachedPageEventsQuery(store, req, &first.Rows[0].Position)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		details = append(details, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	plan := strings.Join(details, "\n")
	for _, want := range []string{"USING COVERING INDEX cached_events_timed_range_idx", "USING COVERING INDEX cached_events_date_range_idx"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("missing %s: %s", want, plan)
		}
	}
	if !strings.Contains(q, "r.start_at>=?") {
		t.Fatal("timed cursor is not an index range bound")
	}
	dayCursor := second.Rows[1].Position
	q, _, err = cachedPageEventsQuery(store, req, &dayCursor)
	if err != nil || !strings.Contains(q, "r.start_date>=?") || strings.Contains(q, "timed_range AS MATERIALIZED") {
		t.Fatalf("date cursor query: %s %v", q, err)
	}
	// Numeric timestamps can sort before a valid cursor while their text casts
	// sort after it. Both SQLite numeric storage classes must trigger fallback.
	for _, number := range []any{int64(3000), 3000.25} {
		if _, err := store.db.ExecContext(ctx, `UPDATE cached_events SET start_at=? WHERE event_id='same' AND calendar_id=?`, number, ids[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadCachedPage(ctx, req, &first.Rows[0].Position); !errors.Is(err, ErrInvalidCachedPageData) {
			t.Fatalf("numeric timestamp %v (%T) after cursor: %v", number, number, err)
		}
	}
}

func TestCachedPageRejectsDisabledSourceAndPreviousSelection(t *testing.T) {
	store, now, ids := newCachedPagesStore(t, [3]string{"account", "calendar", "google"})
	ctx := t.Context()
	req := CachedPageRequest{CalendarIDs: ids, PageSize: 5, Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	previous, err := store.CaptureCachedPageAuthority(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCalendarEnabled(ctx, ids[0], false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCachedPage(ctx, req, nil); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("disabled calendar page: %v", err)
	}
	if err := store.ValidateAuthorityTickets(ctx, previous); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("disabled calendar authority: %v", err)
	}
	if err := store.SetCalendarEnabled(ctx, ids[0], true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateAuthorityTickets(ctx, previous); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("previous calendar selection authority: %v", err)
	}
	if _, err := store.ReadCachedPage(ctx, req, nil); err != nil {
		t.Fatalf("newly enabled calendar page: %v", err)
	}
}
