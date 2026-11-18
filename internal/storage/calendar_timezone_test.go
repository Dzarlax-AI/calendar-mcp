package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestRepairCalendarTimeZone(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateConnection(ctx, Connection{ID: "zone-account", Provider: "google", AccountFingerprint: "zone-account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCalendar(ctx, Calendar{ID: "zone-calendar", ConnectionID: "zone-account", ProviderCalendarID: "primary", Name: "Synthetic", Timezone: "Europe/Belgrade", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if zone, err := s.RepairCalendarTimeZone(ctx, "zone-calendar"); err != nil || zone != "Europe/Belgrade" {
		t.Fatalf("zone=%q err=%v", zone, err)
	}
	if _, err := s.RepairCalendarTimeZone(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing calendar error = %v", err)
	}
}

func TestSQLiteDiscoveryTimezonePreservation(t *testing.T) {
	s := newSQLiteStore(t)
	runDiscoveryTimezonePreservation(t, s)
}
func runDiscoveryTimezonePreservation(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	id := fmt.Sprintf("discovery-zone-%d", now.UnixNano())
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	cal := Calendar{ID: calendar.AccountCalendarID("google", id, "owner"), ConnectionID: id, ProviderCalendarID: "owner", Name: "Synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}
	for _, step := range []struct{ input, want string }{{"Europe/Belgrade", "Europe/Belgrade"}, {"", "Europe/Belgrade"}, {"Local", "Europe/Belgrade"}, {"bad-zone", "Europe/Belgrade"}, {"UTC+05:30:17", "Europe/Belgrade"}, {"America/Los_Angeles", "America/Los_Angeles"}} {
		cal.Timezone = step.input
		if err := s.UpsertCalendar(ctx, cal); err != nil {
			t.Fatal(err)
		}
		got, err := s.RepairCalendarTimeZone(ctx, cal.ID)
		if err != nil || got != step.want {
			t.Fatalf("timezone got=%q want=%q err=%v", got, step.want, err)
		}
	}
	item := calendar.Calendar{ID: "discovered", Name: "Synthetic", Timezone: "Asia/Tokyo"}
	if err := s.RegisterDiscoveredCalendar(ctx, "google@"+id, item); err != nil {
		t.Fatal(err)
	}
	got, err := s.RepairCalendarTimeZone(ctx, calendar.AccountCalendarID("google", id, item.ID))
	if err != nil || got != "Asia/Tokyo" {
		t.Fatalf("discovery timezone %q err %v", got, err)
	}
}

func TestSQLiteAppleTimezoneRefresh(t *testing.T) {
	runAppleTimezoneRefresh(t, newSQLiteStore(t))
}

func runAppleTimezoneRefresh(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	const conn = "apple-offset-fixture"
	if err := s.CreateConnection(ctx, Connection{ID: conn, Provider: "apple", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.CredentialSnapshot(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"first", "second"} {
		c := Calendar{ID: calendar.AccountCalendarID("apple", conn, raw), ConnectionID: conn, ProviderCalendarID: raw, Name: raw, CanRead: true, DiscoveredAt: now}
		if err := s.UpsertCalendar(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListCalendars(ctx, conn)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list sources: %d %v", len(rows), err)
	}
	first, second := rows[0], rows[1]
	const east, west = "UTC+05:30:17", "UTC-00:19:32"
	updates := []AppleCalendarTimezoneUpdate{{first, east}, {second, west}}
	assertZone := func(id, want string) {
		t.Helper()
		if got, err := s.RepairCalendarTimeZone(ctx, id); err != nil || got != want {
			t.Fatalf("zone=%q want=%q err=%v", got, want, err)
		}
	}
	// Grant changes after discovery reject the entire batch, including earlier rows.
	if err := s.SetCalendarEnabled(ctx, second.ID, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, updates); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("changed selection: %v", err)
	}
	assertZone(first.ID, "")
	assertZone(second.ID, "")
	if err := s.SetCalendarEnabled(ctx, second.ID, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, updates); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("re-enabled source accepted stale receipt: %v", err)
	}
	rows, err = s.ListCalendars(ctx, conn)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list re-enabled sources: %d %v", len(rows), err)
	}
	updates[1].Before = rows[1]

	if err := s.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Model an existing CalDAV permission denial with unchanged calendar flags.
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET last_error_code='permission' WHERE calendar_id=?`), second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, updates); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("permission denial: %v", err)
	}
	assertZone(first.ID, "")
	assertZone(second.ID, "")
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET last_error_code=NULL WHERE calendar_id=?`), second.ID); err != nil {
		t.Fatal(err)
	}

	if count, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, updates); err != nil || count != 2 {
		t.Fatalf("refresh: %d %v", count, err)
	}
	assertZone(first.ID, east)
	assertZone(second.ID, west)
	if _, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, updates); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale timezone accepted: %v", err)
	}
	after, err := s.ListCalendars(ctx, conn)
	if err != nil || len(after) != 2 {
		t.Fatalf("list refreshed sources: %d %v", len(after), err)
	}
	for i := range updates {
		want := updates[i].Before
		want.Timezone = updates[i].Timezone
		if after[i].DetailReceipt == want.DetailReceipt || after[i].DetailReceipt == "" {
			t.Fatal("timezone change did not rotate the source receipt")
		}
		want.DetailReceipt = after[i].DetailReceipt
		if after[i] != want {
			t.Fatalf("refresh changed other metadata: %+v, want %+v", after[i], want)
		}
		updates[i].Before = after[i]
	}
	if count, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, updates); err != nil || count != 0 {
		t.Fatalf("idempotent refresh: %d %v", count, err)
	}
	const restoredZone = "UTC"
	stale := []AppleCalendarTimezoneUpdate{{Before: after[0], Timezone: restoredZone}}
	if count, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, stale); err != nil || count != 1 {
		t.Fatalf("intervening change: %d %v", count, err)
	}
	rows, err = s.ListCalendars(ctx, conn)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list changed sources: %d %v", len(rows), err)
	}
	if count, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, []AppleCalendarTimezoneUpdate{{Before: rows[0], Timezone: east}}); err != nil || count != 1 {
		t.Fatalf("restore original offset: %d %v", count, err)
	}
	if _, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, stale); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale refresh accepted after timezone restoration: %v", err)
	}
	assertZone(first.ID, east)

	// An intervening change from ordinary Apple discovery must fence the same
	// in-flight refresh when the zone cycles back to the original value.
	rows, err = s.ListCalendars(ctx, conn)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list restored sources: %d %v", len(rows), err)
	}
	stale[0].Before = rows[0]
	for _, zone := range []string{restoredZone, east} {
		row := rows[0]
		row.Timezone = zone
		if err := s.UpsertCalendar(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, stale); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale refresh accepted after ordinary discovery restored the timezone: %v", err)
	}
	assertZone(first.ID, east)
	rows, err = s.ListCalendars(ctx, conn)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list sources before credential change: %d %v", len(rows), err)
	}
	for i := range updates {
		updates[i].Before = rows[i]
	}
	if err := s.CompareAndSwapCredentials(ctx, snapshot, []byte("replaced"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAppleCalendarTimezones(ctx, snapshot, updates); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale credentials accepted: %v", err)
	}
	verifyAppleCoverageWindow(t, s, first.ID, now, time.Date(2026, 10, 24, 18, 29, 43, 0, time.UTC))
	verifyAppleCoverageWindow(t, s, second.ID, now, time.Date(2026, 10, 25, 0, 19, 32, 0, time.UTC))
}

func verifyAppleCoverageWindow(t *testing.T, s *Store, id string, now, midnight time.Time) {
	t.Helper()
	ctx := t.Context()
	exact := SyncWindow{Start: midnight, End: midnight.Add(24 * time.Hour)}
	late := SyncWindow{Start: exact.Start.Add(time.Second), End: exact.End}
	early := SyncWindow{Start: exact.Start, End: exact.End.Add(-time.Second)}
	req := CachedPageRequest{
		CalendarIDs: []string{id}, Start: midnight.Add(time.Hour), End: midnight.Add(2 * time.Hour),
		PageSize: 2, WithCoverage: true, StartDate: "2026-10-25", EndDate: "2026-10-26",
	}
	event := calendar.EventV2{ID: "synthetic-day", CalendarID: id, Provider: "apple", Title: "Synthetic",
		Start: calendar.EventTime{Date: req.StartDate}, End: calendar.EventTime{Date: req.EndDate}}
	if err := s.UpsertCachedEvent(ctx, event, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stored, target SyncWindow
		want           string
	}{
		{exact, exact, "covered"},
		{late, exact, "outside_window"},
		{early, exact, "outside_window"},
		{exact, late, "outside_window"},
		{exact, early, "outside_window"},
	} {
		req.TargetWindow = tc.target
		_, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET status='ready',last_success_at=?,next_sync_at=?,window_start=?,window_end=? WHERE calendar_id=?`),
			now, now.Add(time.Hour), tc.stored.Start, tc.stored.End, id)
		if err != nil {
			t.Fatal(err)
		}
		page, err := s.ReadCachedPage(ctx, req, nil)
		if err != nil || len(page.Sources) != 1 || page.Sources[0].Coverage != tc.want ||
			len(page.Rows) != 1 || page.Rows[0].Summary.EventID != event.ID {
			t.Fatalf("coverage with stored=%+v target=%+v, want %s: %+v %v", tc.stored, tc.target, tc.want, page, err)
		}
	}
}
