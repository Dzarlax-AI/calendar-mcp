//go:build integration

package storage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestPostgresCachedPageSnapshotProjectionAndKeysetParity(t *testing.T) {
	store := newAuthorityPostgresStore(t, SchemaVersion)
	ctx := t.Context()
	now := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	const connectionID = "cached-pages-parity"
	window := SyncWindow{Start: now.AddDate(-1, 0, 0), End: now.AddDate(2, 0, 0)}
	if err := store.CreateConnection(ctx, Connection{ID: connectionID, Provider: "google", DisplayName: "Synthetic pages", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	const scopeSize = 16
	ids := make([]string, scopeSize)
	staged := make([]DiscoveredDetailCalendar, scopeSize)
	for i := range ids {
		providerID := fmt.Sprintf("calendar-%02d", i)
		ids[i] = "google:" + connectionID + ":" + providerID
		staged[i] = DiscoveredDetailCalendar{Calendar: Calendar{ID: ids[i], ConnectionID: connectionID, ProviderCalendarID: providerID, Name: "Synthetic"}, AccessRole: "reader"}
	}
	snapshot, err := store.BeginAuthorization(ctx, connectionID, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteGoogleAuthorization(ctx, snapshot, staged, now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	start := now.Add(-time.Hour)
	end := now.Add(24 * time.Hour)
	request := CachedPageRequest{CalendarIDs: ids, Start: start, End: end, PageSize: 1}

	timedStart := now.Add(time.Hour)
	timedEnd := timedStart.Add(30 * time.Minute)
	items := []struct {
		id, calendarID string
		start, end     calendar.EventTime
		instanceKind   string
		description    string
	}{
		{id: "00-series-master", calendarID: ids[0], start: calendar.EventTime{DateTime: now.Add(30 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, end: calendar.EventTime{DateTime: now.Add(45 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, instanceKind: "seriesMaster"},
		{id: "a-time", calendarID: ids[0], start: calendar.EventTime{DateTime: timedStart.Format(time.RFC3339), TimeZone: "UTC"}, end: calendar.EventTime{DateTime: timedEnd.Format(time.RFC3339), TimeZone: "UTC"}, description: strings.Repeat("private-description-", 6000)},
		{id: "b-time", calendarID: ids[1], start: calendar.EventTime{DateTime: timedStart.Format(time.RFC3339), TimeZone: "UTC"}, end: calendar.EventTime{DateTime: timedEnd.Format(time.RFC3339), TimeZone: "UTC"}},
		{id: "same-time", calendarID: ids[0], start: calendar.EventTime{DateTime: timedStart.Format(time.RFC3339), TimeZone: "UTC"}, end: calendar.EventTime{DateTime: timedEnd.Format(time.RFC3339), TimeZone: "UTC"}},
		{id: "same-time", calendarID: ids[1], start: calendar.EventTime{DateTime: timedStart.Format(time.RFC3339), TimeZone: "UTC"}, end: calendar.EventTime{DateTime: timedEnd.Format(time.RFC3339), TimeZone: "UTC"}},
		{id: "a-day", calendarID: ids[0], start: calendar.EventTime{Date: "2026-08-22"}, end: calendar.EventTime{Date: "2026-08-23"}},
		{id: "b-day", calendarID: ids[1], start: calendar.EventTime{Date: "2026-08-22"}, end: calendar.EventTime{Date: "2026-08-23"}},
		{id: "same-day", calendarID: ids[0], start: calendar.EventTime{Date: "2026-08-22"}, end: calendar.EventTime{Date: "2026-08-23"}},
		{id: "same-day", calendarID: ids[1], start: calendar.EventTime{Date: "2026-08-22"}, end: calendar.EventTime{Date: "2026-08-23"}},
		{id: "z-jumbo", calendarID: ids[2], start: calendar.EventTime{DateTime: now.Add(4 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, end: calendar.EventTime{DateTime: now.Add(5 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}},
	}
	for _, item := range items {
		event := calendar.EventV2{ID: item.id, CalendarID: item.calendarID, Provider: "google", Title: item.id, Description: item.description, InstanceKind: item.instanceKind, Start: item.start, End: item.end}
		if item.id == "z-jumbo" {
			event.Title = strings.Repeat("oversized-title-", 3000)
		}
		if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
	}

	// A stray date on a timed record is ignored for key ordering and cursors.
	if _, err := store.db.ExecContext(ctx, `UPDATE cached_events SET start_date='2026-08-21' WHERE event_id IN ('a-time','b-time')`); err != nil {
		t.Fatal(err)
	}

	// The raw source is bounded by SQL projection: huge unrelated descriptions
	// do not cross the driver boundary, and a huge projected title becomes a
	// marker that retains the row's identity and time range.
	var after *CachedPagePosition
	var walked []string
	var sawJumbo bool
	for {
		page, err := store.ReadCachedPage(ctx, request, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) > request.PageSize+1 {
			t.Fatalf("page query exceeded page_size + 1: %d", len(page.Rows))
		}
		if len(page.Sources) != scopeSize || len(page.Tickets) != scopeSize {
			t.Fatalf("source snapshot lost calendars: sources=%d tickets=%d", len(page.Sources), len(page.Tickets))
		}
		if after == nil {
			if err := store.ValidateAuthorityTickets(ctx, page.Tickets); err != nil {
				t.Fatalf("batch authority: %v", err)
			}
			duplicates := []AuthorityTicket{page.Tickets[0], page.Tickets[0]}
			if err := store.ValidateAuthorityTickets(ctx, duplicates); err != nil {
				t.Fatalf("duplicate authority: %v", err)
			}
			bad := append([]AuthorityTicket(nil), page.Tickets...)
			bad[15].DetailReceipt = page.Tickets[0].DetailReceipt
			if err := store.ValidateAuthorityTickets(ctx, bad); !errors.Is(err, ErrAuthorityChanged) {
				t.Fatalf("mismatched last authority: %v", err)
			}
		}
		returned := page.Rows
		if len(returned) > request.PageSize {
			returned = returned[:request.PageSize]
		}
		for _, row := range returned {
			walked = append(walked, row.Summary.EventID)
			if strings.HasSuffix(row.Summary.EventID, "-day") && (!row.Position.AllDay || row.Position.StartDate != "2026-08-22" || row.Position.StartAt != "") {
				t.Fatalf("PostgreSQL all-day key mismatch: %+v", row.Position)
			}
			if row.Summary.EventID == "a-time" && (row.Summary.Kind != "event" || row.Summary.Title != "a-time") {
				t.Fatalf("projected normal summary differs: %+v", row.Summary)
			}
			if row.Summary.EventID == "z-jumbo" {
				sawJumbo = row.Summary.Kind == "oversized" && row.Summary.Title == "" && row.Summary.Start.DateTime != "" && row.Summary.Reason == "summary_projection_too_large"
			}
		}
		if page.Stats.PayloadBytes > 16<<10 || page.Stats.MaxPayloadBytes > 16<<10 {
			t.Fatalf("unrelated source payload crossed the projection boundary: %+v", page.Stats)
		}
		if len(page.Rows) <= request.PageSize {
			break
		}
		after = &returned[len(returned)-1].Position
	}
	want := []string{"a-time", "b-time", "same-time", "same-time", "z-jumbo", "a-day", "b-day", "same-day", "same-day"}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("PostgreSQL order/keyset walk mismatch: got=%v want=%v", walked, want)
	}
	if !sawJumbo {
		t.Fatal("oversized projected summary did not become an explicit marker")
	}
	first, err := store.ReadCachedPage(ctx, request, nil)
	if err != nil || len(first.Rows) != 2 || first.Rows[0].Summary.EventID != "a-time" || first.Rows[1].Summary.EventID != "b-time" {
		t.Fatalf("equal-instant timed ordering / series-master-before-limit check failed: rows=%#v err=%v", first.Rows, err)
	}
	if first.Rows[0].Position.StartAt == "" || first.Rows[0].Position.StartDate != "" || first.Rows[0].Position.StartAt != first.Rows[1].Position.StartAt {
		t.Fatalf("PostgreSQL timed cursor was not canonical: %#v %#v", first.Rows[0].Position, first.Rows[1].Position)
	}
	request.PageSize = 10
	first, err = store.ReadCachedPage(ctx, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, expectedCalendar := range []string{ids[0], ids[1]} {
		if first.Rows[2+i].Summary.EventID != "same-time" || first.Rows[2+i].Position.CalendarID != expectedCalendar {
			t.Fatalf("timed calendar tie order mismatch at %d: %#v", i, first.Rows[2+i])
		}
	}
	for i, expectedCalendar := range []string{ids[0], ids[1]} {
		if first.Rows[7+i].Summary.EventID != "same-day" || first.Rows[7+i].Position.CalendarID != expectedCalendar {
			t.Fatalf("date calendar tie order mismatch at %d: %#v", i, first.Rows[7+i])
		}
	}

	// Whole-scope failures and closed proofs do not return partial sources.
	partial := request
	partial.CalendarIDs = append([]string{}, ids...)
	partial.CalendarIDs[len(partial.CalendarIDs)-1] = "google:cached-pages-parity:missing"
	if _, err := store.CaptureCachedPageAuthority(ctx, partial); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("partial unknown scope error=%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM calendar_sync_state WHERE calendar_id=$1`, ids[15]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureCachedPageAuthority(ctx, request); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("missing sync state error=%v", err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE calendars SET detail_receipt='' WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureCachedPageAuthority(ctx, request); !errors.Is(err, ErrCachedPageUnavailable) {
		t.Fatalf("closed receipt error=%v", err)
	}
}
