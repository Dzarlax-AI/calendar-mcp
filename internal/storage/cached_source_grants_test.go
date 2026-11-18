package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestCachedPresentationChecksGrantAfterDecode(t *testing.T) {
	for _, fixture := range []struct {
		name, provider string
		streaming      bool
	}{{"google_buffered", "google", false}, {"google_streaming", "google", true}, {"apple", "apple", false}, {"microsoft", "microsoft", false}} {
		t.Run(fixture.name, func(t *testing.T) {
			dsn := "sqlite://" + filepath.Join(t.TempDir(), "grants.db")
			reader, err := Open(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if err := reader.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			writer, err := Open(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			testCachedPresentationReconnect(t, reader, writer, fixture.provider, fixture.streaming)
		})
	}
}

func testCachedPresentationReconnect(t *testing.T, reader, writer *Store, provider string, streaming bool) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	end := now.Add(time.Hour)
	for _, account := range []string{"first", "second"} {
		if err := writer.CreateConnection(ctx, Connection{ID: account, Provider: provider, DisplayName: account, Status: "connected",
			EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	ready := func(account string, ids ...string) error {
		snapshot, err := writer.BeginAuthorization(ctx, account, []byte("synthetic"), now)
		if err != nil {
			return err
		}
		calendars := make([]Calendar, 0, len(ids))
		for _, id := range ids {
			calendars = append(calendars, Calendar{ID: id, ConnectionID: account, ProviderCalendarID: id, Name: id, CanRead: true})
		}
		if provider != "google" {
			return writer.CompleteNonGoogleAuthorization(ctx, snapshot, calendars, account+"-fingerprint", account, now)
		}
		items := make([]DiscoveredDetailCalendar, 0, len(calendars))
		for _, source := range calendars {
			items = append(items, DiscoveredDetailCalendar{Calendar: source, AccessRole: "owner"})
		}
		return writer.CompleteGoogleAuthorization(ctx, snapshot, items, now)
	}
	if err := ready("first", "changed", "empty"); err != nil {
		t.Fatal(err)
	}
	if err := ready("second", "healthy"); err != nil {
		t.Fatal(err)
	}
	if err := writer.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	seeding := func(calendarID, id string) error {
		event := cachedTimedEvent(id, now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
		event.CalendarID, event.Provider = calendarID, provider
		if streaming && id == "old" {
			event.Description = strings.Repeat("x", 8<<20)
		}
		return writer.UpsertCachedEvent(ctx, event, now)
	}
	if err := seeding("changed", "old"); err != nil {
		t.Fatal(err)
	}
	if err := seeding("healthy", "keep"); err != nil {
		t.Fatal(err)
	}
	// The read grant is keyed by the SQL row rather than the calendar ID
	// serialized inside its payload.
	payload := cachedTimedEvent("old", now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
	payload.CalendarID = "healthy"
	if streaming {
		payload.Description = strings.Repeat("x", 8<<20)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.db.ExecContext(ctx, writer.query(`UPDATE cached_events SET payload_json=? WHERE calendar_id=?`), string(raw), "changed"); err != nil {
		t.Fatal(err)
	}
	probe := &decodeProbeContext{Context: ctx, probe: func() error {
		if reader.dialect == DialectSQLite && !streaming && reader.db.Stats().InUse != 0 {
			t.Error("buffered decode still holds the SQLite connection")
		}
		if err := ready("first", "changed", "empty"); err != nil {
			return err
		}
		return seeding("changed", "new")
	}}
	ids := []string{"changed", "healthy", "empty"}
	events, statuses, err := reader.ListVisibleCachedEvents(probe, ids, start, end)
	if err != nil || !probe.seen.Load() || len(events) != 1 || events[0].ID != "keep" {
		t.Fatalf("old snapshot returned %d items, first item healthy = %v, err=%v", len(events), len(events) > 0 && events[0].ID == "keep", err)
	}
	for _, id := range []string{"changed", "empty"} {
		if !slices.ContainsFunc(statuses, func(s CachedSourceStatus) bool { return s.CalendarID == id && s.Status == "unavailable" && s.Stale }) {
			t.Fatalf("source %s was not marked unavailable: %+v", id, statuses)
		}
	}
	current, _, err := reader.ListVisibleCachedEvents(ctx, ids, start, end)
	if err != nil || len(current) != 2 || !slices.ContainsFunc(current, func(e calendar.EventV2) bool { return e.ID == "new" }) {
		t.Fatalf("fresh read after reconnect returned %d items, err=%v", len(current), err)
	}
}

func TestCachedPresentationFailsIfGrantCheckFails(t *testing.T) {
	store, now := newEventCacheStore(t)
	if err := store.UpsertCachedEvent(t.Context(), cachedTimedEvent("event", now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339)), now); err != nil {
		t.Fatal(err)
	}
	probe := &decodeProbeContext{Context: context.Background(), probe: store.Close}
	events, statuses, err := store.ListVisibleCachedEvents(probe, []string{"calendar"}, now, now.Add(time.Hour))
	if err == nil || len(events) != 0 || len(statuses) != 0 || !probe.seen.Load() {
		t.Fatalf("expected a failed recheck with no response, err=%v", err)
	}
}

func TestCachedPresentationReportsEveryRequestedSource(t *testing.T) {
	store := newSQLiteStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.CreateConnection(ctx, Connection{ID: "account", Provider: "microsoft", DisplayName: "account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"healthy", "unwarmed", "revoked"} {
		if err := store.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: id != "revoked", DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now, End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, store.query(`DELETE FROM calendar_sync_state WHERE calendar_id=?`), "unwarmed"); err != nil {
		t.Fatal(err)
	}
	event := cachedTimedEvent("keep", now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
	event.CalendarID = "healthy"
	if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
		t.Fatal(err)
	}
	events, statuses, err := store.ListVisibleCachedEvents(ctx, []string{"healthy", "revoked", "unwarmed", "missing", "revoked"}, now, now.Add(time.Hour))
	if err != nil || len(events) != 1 || events[0].ID != "keep" || len(statuses) != 4 {
		t.Fatalf("events=%+v statuses=%+v err=%v", events, statuses, err)
	}
	for _, status := range statuses {
		want := "pending"
		if status.CalendarID == "revoked" || status.CalendarID == "missing" {
			want = "unavailable"
			if status.Provider != "" {
				t.Fatalf("unavailable source exposed provider: %+v", status)
			}
		}
		if status.Status != want || !status.Stale {
			t.Fatalf("unexpected status: %+v", status)
		}
	}
	probe := &decodeProbeContext{Context: ctx, probe: func() error {
		return store.UpsertCalendar(ctx, Calendar{ID: "revoked", ConnectionID: "account", ProviderCalendarID: "revoked", Name: "revoked", CanRead: true, DiscoveredAt: now})
	}}
	events, statuses, err = store.ListVisibleCachedEvents(probe, []string{"healthy", "revoked"}, now, now.Add(time.Hour))
	if err != nil || !probe.seen.Load() || len(events) != 1 || !slices.ContainsFunc(statuses, func(s CachedSourceStatus) bool {
		return s.CalendarID == "revoked" && s.Status == "unavailable" && s.Stale
	}) {
		t.Fatalf("regrant during read: events=%+v statuses=%+v err=%v", events, statuses, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	events, statuses, err = store.ListVisibleCachedEvents(canceled, []string{"healthy"}, now, now.Add(time.Hour))
	if err == nil || events != nil || statuses != nil {
		t.Fatalf("canceled read: events=%+v statuses=%+v err=%v", events, statuses, err)
	}
}

func TestCachedPresentationKeepsSiblingOnSourceGrantChange(t *testing.T) {
	store := newSQLiteStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	const account = "account"
	if err := store.CreateConnection(ctx, Connection{ID: account, Provider: "microsoft", DisplayName: account, Status: "connected",
		EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ids := []string{"changed", "healthy"}
	for _, id := range ids {
		if err := store.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: account, ProviderCalendarID: id, Name: id, CanRead: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now, End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		event := cachedTimedEvent(id, now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
		event.CalendarID, event.Provider = id, "microsoft"
		if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
	}
	probe := &decodeProbeContext{Context: ctx, probe: func() error {
		source := Calendar{ID: "changed", ConnectionID: account, ProviderCalendarID: "changed", Name: "changed", DiscoveredAt: now}
		if err := store.UpsertCalendar(ctx, source); err != nil {
			return err
		}
		source.CanRead = true
		if err := store.UpsertCalendar(ctx, source); err != nil {
			return err
		}
		snapshot, err := store.CredentialSnapshot(ctx, account)
		if err != nil {
			return err
		}
		if err := store.CompareAndSwapCredentials(ctx, snapshot, []byte("rotated synthetic"), now); err != nil {
			return err
		}
		_, err = store.ClaimDueCalendarSync(ctx, "synthetic", now, now.Add(time.Minute))
		return err
	}}
	events, statuses, err := store.ListVisibleCachedEvents(probe, ids, now, now.Add(time.Hour))
	if err != nil || !probe.seen.Load() || len(events) != 1 || events[0].ID != "healthy" {
		t.Fatalf("sibling read returned %+v, err=%v", events, err)
	}
	if len(statuses) != 2 || statuses[0].CalendarID != "changed" || statuses[0].Status != "unavailable" || statuses[1].Status == "unavailable" {
		t.Fatalf("source statuses = %+v", statuses)
	}
}

func TestCachedPresentationFencesSelectionToggleDuringDecode(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		name := "verified"
		if migrated {
			name = "migrated"
		}
		t.Run(name, func(t *testing.T) {
			store, now := newEventCacheStore(t)
			ctx := t.Context()
			if migrated {
				markConnectionMigrated(t, store, "connection")
			} else {
				snapshot, err := store.BeginAuthorization(ctx, "connection", nil, now)
				if err != nil {
					t.Fatal(err)
				}
				entry := DiscoveredDetailCalendar{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary"}, AccessRole: "reader"}
				if err := store.CompleteGoogleAuthorization(ctx, snapshot, []DiscoveredDetailCalendar{entry}, now); err != nil {
					t.Fatal(err)
				}
			}
			event := cachedTimedEvent("selection", now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
			if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
				t.Fatal(err)
			}
			probe := &decodeProbeContext{Context: ctx, probe: func() error {
				if err := store.SetCalendarEnabled(ctx, "calendar", false, now); err != nil {
					return err
				}
				return store.SetCalendarEnabled(ctx, "calendar", true, now)
			}}
			ids := []string{"calendar"}
			events, statuses, err := store.ListVisibleCachedEvents(probe, ids, now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil || !probe.seen.Load() || len(events) != 0 || len(statuses) != 1 || statuses[0].Status != "unavailable" {
				t.Fatalf("in-flight read: probe=%v events=%d statuses=%+v err=%v", probe.seen.Load(), len(events), statuses, err)
			}
			events, _, err = store.ListVisibleCachedEvents(ctx, ids, now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil || len(events) != 1 || events[0].ID != "selection" {
				t.Fatalf("fresh read: events=%d err=%v", len(events), err)
			}
		})
	}
}
