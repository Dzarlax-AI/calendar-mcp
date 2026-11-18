package cachedpages

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

func newCoverageFixture(t *testing.T, size int) serviceFixture {
	t.Helper()
	return newCoverageFixtureAt(t, size, "sqlite://"+filepath.Join(t.TempDir(), "coverage.db"))
}

func newCoverageFixtureAt(t *testing.T, size int, databaseURL string) serviceFixture {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.FixedZone("summer", 2*3600))
	db, err := storage.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	window := storage.SyncWindow{Start: now.AddDate(0, 0, -2), End: now.AddDate(0, 0, 3)}
	for i := 0; i < size; i++ {
		provider := coverageProvider(i)
		connection, rawID := fmt.Sprintf("conn-%02d", i), "calendar"
		model := storage.Calendar{ID: provider + ":" + connection + ":" + rawID, ProviderCalendarID: rawID, ConnectionID: connection, Name: "Synthetic", Timezone: "UTC", CanRead: true, DiscoveredAt: now}
		if err := db.CreateConnection(ctx, storage.Connection{ID: connection, Provider: provider, DisplayName: "Synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if provider == "google" {
			snapshot, err := db.BeginAuthorization(ctx, connection, nil, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteGoogleAuthorization(ctx, snapshot, []storage.DiscoveredDetailCalendar{{Calendar: model, AccessRole: "reader"}}, now); err != nil {
				t.Fatal(err)
			}
		} else if err := db.UpsertCalendar(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < size; i++ {
		state := claimCoverageSource(t, db, now)
		index := 0
		if _, err := fmt.Sscanf(state.CalendarID, coverageProviderPrefix(state.CalendarID)+":conn-%d:calendar", &index); err != nil {
			t.Fatal(err)
		}
		event := coverageEvent(state.CalendarID, fmt.Sprintf("item-%02d", index), now.Add(3*time.Hour))
		if index == 0 {
			event.Start = calendar.EventTime{Date: "2026-10-25"}
			event.End = calendar.EventTime{Date: "2026-10-26"}
		}
		master := coverageEvent(state.CalendarID, "master", now.Add(time.Hour))
		master.InstanceKind = "seriesMaster"
		master.Title = strings.Repeat("wide", 10000)
		if index == 7 && size == 8 {
			if err := db.ParkCalendarSync(ctx, *state, "permission", now); err != nil {
				t.Fatal(err)
			}
			// Model a parked row left over from a previous release.
			if err := db.UpsertCachedEvent(ctx, event, now); err != nil {
				t.Fatal(err)
			}
			continue
		}
		finishCoverageSource(t, db, state, now, event, master)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(db, cipher, Options{Window: window, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return serviceFixture{store: db, svc: svc, now: now, ctx: verifiedContext(t, "coverage-test", ""), page: Request{
		Start: now, End: time.Date(2026, 10, 26, 0, 0, 0, 0, time.FixedZone("winter", 3600)), PageSize: 2,
	}}
}

func coverageProviderPrefix(id string) string {
	provider, _, _ := strings.Cut(id, ":")
	return provider
}

func coverageProvider(index int) string {
	switch {
	case index < 5:
		return "google"
	case index < 8:
		return "apple"
	default:
		return "microsoft"
	}
}

func coverageEvent(id, eventID string, when time.Time) calendar.EventV2 {
	return calendar.EventV2{ID: eventID, CalendarID: id, Provider: coverageProviderPrefix(id), Title: eventID,
		Description: "synthetic meeting details", Status: "confirmed",
		Start: calendar.EventTime{DateTime: when.UTC().Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: when.Add(time.Hour).UTC().Format(time.RFC3339), TimeZone: "UTC"}}
}

func claimCoverageSource(t *testing.T, db *storage.Store, now time.Time) *storage.CalendarSyncState {
	t.Helper()
	state, err := db.ClaimDueCalendarSync(t.Context(), "coverage-worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim synthetic source: %v", err)
	}
	return state
}

func finishCoverageSource(t *testing.T, db *storage.Store, state *storage.CalendarSyncState, now time.Time, events ...calendar.EventV2) {
	t.Helper()
	next := now.Add(time.Hour)
	batch := storage.EventSyncBatch{FullSync: true, NextCursor: "synthetic", NextSyncAt: &next}
	for _, event := range events {
		batch.Upserts = append(batch.Upserts, storage.CachedEventUpsert{Event: event})
	}
	if err := db.ApplyEventSyncPage(t.Context(), *state, batch, true, now); err != nil {
		t.Fatal(err)
	}
}

func walkCoverage(t *testing.T, f serviceFixture) ([]string, Page) {
	t.Helper()
	var ids []string
	var token string
	for round := 0; round < 128; round++ {
		page, _, err := f.svc.ReadCoverage(f.ctx, f.page, token)
		if err != nil {
			t.Fatal(err)
		}
		if _, size, err := NewToolResult(page); err != nil || size > MaxResultBytes {
			t.Fatalf("page exceeds wire budget: %d %v", size, err)
		}
		for _, event := range page.Items {
			ids = append(ids, event.EventID)
		}
		token = page.NextPageToken
		if token == "" {
			return ids, page
		}
		if page.Complete {
			t.Fatal("continuation marked complete")
		}
	}
	t.Fatal("walk exceeded bound")
	return nil, Page{}
}

func TestCoverageWalkParkedAppleAndRecovery(t *testing.T) {
	f := newCoverageFixture(t, 8)
	ids, final := walkCoverage(t, f)
	expected := []string{"item-01", "item-02", "item-03", "item-04", "item-05", "item-06", "item-00"}
	if !reflect.DeepEqual(ids, expected) || len(final.Sources) != 8 || !final.Partial || final.Complete {
		t.Fatalf("coverage results %v, partial=%t complete=%t sources=%d", ids, final.Partial, final.Complete, len(final.Sources))
	}
	id := "apple:conn-07:calendar"
	for _, source := range final.Sources {
		if source.CalendarID == id {
			if source.Coverage != "unavailable" || source.ErrorCode != "permission" || source.LastSuccessAt != nil {
				t.Fatalf("permission source metadata: %+v", source)
			}
		} else if source.Coverage != "covered" {
			t.Fatalf("healthy source not covered: %+v", source)
		}
	}
	first, _, err := f.svc.ReadCoverage(f.ctx, f.page, "")
	if err != nil || first.NextPageToken == "" {
		t.Fatalf("first page: %v", err)
	}
	if err := f.store.ScheduleCalendarSync(t.Context(), id, f.now, false); err != nil {
		t.Fatal(err)
	}
	ids, _ = walkCoverage(t, f)
	if !reflect.DeepEqual(ids, expected) {
		t.Fatalf("pending recovery released parked rows: %v", ids)
	}
	state := claimCoverageSource(t, f.store, f.now)
	finishCoverageSource(t, f.store, state, f.now, coverageEvent(id, "item-07", f.now.Add(3*time.Hour)))
	if _, _, err := f.svc.ReadCoverage(f.ctx, f.page, first.NextPageToken); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("scope recovery accepted previous cursor: %v", err)
	}
	ids, final = walkCoverage(t, f)
	if len(ids) != 8 || final.Partial || !final.Complete {
		t.Fatalf("recovered walk %v, partial=%t complete=%t", ids, final.Partial, final.Complete)
	}
}

func TestCoverageBoundCursorAndFinalRevocation(t *testing.T) {
	f := newCoverageFixture(t, 16)
	first, _, err := f.svc.ReadCoverage(f.ctx, f.page, "")
	if err != nil || first.NextPageToken == "" || len(first.Sources) != 16 {
		t.Fatalf("expected 16 sources and continuation: %v", err)
	}
	other := verifiedContext(t, "other-synthetic-key", "")
	if _, _, err := f.svc.ReadCoverage(other, f.page, first.NextPageToken); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("cursor transferred between callers: %v", err)
	}
	f.svc.testBeforeFinalValidation = func() {
		for _, selected := range []bool{false, true} {
			if err := f.store.SetCalendarEnabled(t.Context(), "microsoft:conn-08:calendar", selected, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	rejected, _, err := f.svc.ReadCoverage(f.ctx, f.page, first.NextPageToken)
	if !errors.Is(err, ErrCursorUnavailable) || len(rejected.Items) != 0 {
		t.Fatalf("revocation crossed final delivery: %v, %d items", err, len(rejected.Items))
	}
	f.svc.testBeforeFinalValidation = nil
	explicit := f.page
	explicit.CalendarIDs = []string{"microsoft:conn-08:calendar"}
	if _, _, err := f.svc.ReadCoverage(f.ctx, explicit, first.NextPageToken); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("cursor accepted new scope: %v", err)
	}
}

func TestCoverageMarkerPersistsThroughTerminalPage(t *testing.T) {
	f := newCoverageFixture(t, 2)
	id := "google:conn-01:calendar"
	event := coverageEvent(id, "wide", f.now.Add(time.Hour))
	event.Title = strings.Repeat("oversized", 10000)
	if err := f.store.UpsertCachedEvent(t.Context(), event, f.now); err != nil {
		t.Fatal(err)
	}
	f.page.PageSize = 1
	first, _, err := f.svc.ReadCoverage(f.ctx, f.page, "")
	if err != nil || !first.Partial || len(first.Items) != 1 || first.Items[0].Kind != "oversized" || first.NextPageToken == "" {
		t.Fatalf("first oversized page: %v %+v", err, first)
	}
	_, final := walkCoverage(t, f)
	if !final.Partial || final.Complete {
		t.Fatalf("terminal page forgot previous marker: %+v", final)
	}
}

func TestCoverageSixteenSourcesWithBackground(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) {
		verifyCoverageWithBackground(t, newCoverageFixture(t, 16))
	})
	t.Run("PostgreSQL", func(t *testing.T) {
		databaseURL := os.Getenv("TEST_POSTGRES_URL")
		if databaseURL == "" {
			t.Skip("TEST_POSTGRES_URL is not configured")
		}
		u, err := url.Parse(databaseURL)
		if err != nil || u.Hostname() != "127.0.0.1" || !strings.HasPrefix(u.Path, "/calendar_r6_") || u.User.Username() != "calendar_r6" {
			t.Fatal("coverage integration requires a disposable loopback calendar_r6 database")
		}
		admin, err := sql.Open("pgx", databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		schema := "coverage_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
			_ = admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
		verifyCoverageWithBackground(t, newCoverageFixtureAt(t, 16, u.String()))
	})
}

func verifyCoverageWithBackground(t *testing.T, f serviceFixture) {
	t.Helper()
	f.page.PageSize = 5
	var calendars []string
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("%s:conn-%02d:calendar", coverageProvider(i), i)
		calendars = append(calendars, id)
		for j := 0; j < 16; j++ {
			event := coverageEvent(id, fmt.Sprintf("extra-%02d-%02d", i, j), f.now.Add(time.Duration(j+5)*time.Hour))
			event.Description = strings.Repeat("synthetic", 250)
			if err := f.store.UpsertCachedEvent(t.Context(), event, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	expected, final := walkCoverage(t, f)
	if len(expected) != 272 || !final.Complete {
		t.Fatalf("unexpected baseline: %d events, complete=%t", len(expected), final.Complete)
	}
	bgTime := f.now.Add(2 * time.Hour)
	state := claimCoverageSource(t, f.store, bgTime)
	_, partial := walkCoverage(t, f)
	if !partial.Partial || partial.Complete {
		t.Fatal("a refresh in progress was reported as covering the source")
	}
	bg := make(chan error, 1)
	go func() {
		_, _, err := f.store.ListCachedEvents(t.Context(), calendars, f.page.Start, f.page.End)
		if err == nil {
			next := bgTime.Add(time.Hour)
			err = f.store.ApplyEventSyncPage(t.Context(), *state, storage.EventSyncBatch{NextCursor: "delta", NextSyncAt: &next}, true, bgTime)
		}
		bg <- err
	}()
	got, _ := walkCoverage(t, f)
	if err := <-bg; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("background sync changed fixed fixture visibility: got %d vs %d", len(got), len(expected))
	}
	got, final = walkCoverage(t, f)
	if !final.Complete || !reflect.DeepEqual(got, expected) {
		t.Fatal("full coverage did not recover after the background write")
	}
}

func TestCoverageDefaultScopeRequiresExplicitSelectionAboveLimit(t *testing.T) {
	f := newCoverageFixture(t, 17)
	if _, _, err := f.svc.ReadCoverage(f.ctx, f.page, ""); !errors.Is(err, storage.ErrExplicitCachedPageScope) {
		t.Fatalf("implicit scope accepted more than sixteen sources: %v", err)
	}
	request := f.page
	request.CalendarIDs = []string{"google:conn-00:calendar"}
	page, _, err := f.svc.ReadCoverage(f.ctx, request, "")
	if err != nil || !page.Complete || len(page.Items) != 1 {
		t.Fatalf("explicit narrow scope: %v %+v", err, page)
	}
	if err := f.store.SetCalendarEnabled(t.Context(), request.CalendarIDs[0], false, f.now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{request.CalendarIDs[0], "google:unknown:calendar"} {
		request.CalendarIDs = []string{id}
		page, _, err := f.svc.ReadCoverage(f.ctx, request, "")
		if !errors.Is(err, ErrUnavailable) || len(page.Items) != 0 || len(page.Sources) != 0 {
			t.Fatalf("unavailable explicit scope was distinguishable: %v %+v", err, page)
		}
	}
}

func TestCoverageFanOutPolicyBeforeLimit(t *testing.T) {
	f := newCoverageFixture(t, 17)
	f.svc.excludeCalendarIDs = []string{"google:calendar"}
	page, _, err := f.svc.ReadCoverage(f.ctx, f.page, "")
	if err != nil || len(page.Sources) != 12 {
		t.Fatalf("legacy exclusion before limit: %v sources=%d", err, len(page.Sources))
	}
	request := f.page
	request.CalendarIDs = []string{"google:conn-00:calendar"}
	page, _, err = f.svc.ReadCoverage(f.ctx, request, "")
	if err != nil || len(page.Sources) != 1 {
		t.Fatalf("explicit bypass: %v %+v", err, page)
	}
	f.svc.excludeCalendarIDs = []string{"microsoft:conn-16:calendar"}
	page, _, err = f.svc.ReadCoverage(f.ctx, f.page, "")
	if err != nil || len(page.Sources) != 16 {
		t.Fatalf("canonical exclusion before limit: %v sources=%d", err, len(page.Sources))
	}
}

func TestCoverageRejectsEmptyCivilRangeButAcceptsDSTDay(t *testing.T) {
	f := newCoverageFixture(t, 1)
	if _, _, err := f.svc.ReadCoverage(f.ctx, f.page, ""); err != nil {
		t.Fatalf("25-hour DST day: %v", err)
	}
	f.page.Start = time.Date(2026, 10, 25, 0, 30, 0, 0, time.FixedZone("east", 14*3600))
	f.page.End = time.Date(2026, 10, 25, 0, 0, 0, 0, time.FixedZone("west", -12*3600))
	if _, _, err := f.svc.ReadCoverage(f.ctx, f.page, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty civil range: %v", err)
	}
}

func TestCoverageUnknownTimezoneIsPartial(t *testing.T) {
	f := newCoverageFixture(t, 1)
	// A separate source without discovered timezone cannot prove civil coverage.
	model := storage.Calendar{ID: "apple:extra:calendar", ProviderCalendarID: "calendar", ConnectionID: "extra", Name: "Synthetic", CanRead: true, DiscoveredAt: f.now}
	if err := f.store.CreateConnection(t.Context(), storage.Connection{ID: "extra", Provider: "apple", DisplayName: "Synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: f.now, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertCalendar(t.Context(), model); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EnsureCalendarSyncStates(t.Context(), f.now, storage.SyncWindow{Start: f.now.AddDate(0, 0, -2), End: f.now.AddDate(0, 0, 3)}); err != nil {
		t.Fatal(err)
	}
	state := claimCoverageSource(t, f.store, f.now)
	finishCoverageSource(t, f.store, state, f.now)
	request := f.page
	request.CalendarIDs = []string{model.ID}
	page, _, err := f.svc.ReadCoverage(f.ctx, request, "")
	if err != nil || !page.Partial || page.Sources[0].Coverage != "outside_window" {
		t.Fatalf("unknown timezone: %v %+v", err, page)
	}
}

func TestCoverageImportedCalendarPolicy(t *testing.T) {
	f := newCoverageFixture(t, 16)
	raw := "feed@import.calendar.google.com"
	id := "google:imported:" + raw
	if err := f.store.CreateConnection(t.Context(), storage.Connection{ID: "imported", Provider: "google", DisplayName: "Synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: f.now, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertCalendar(t.Context(), storage.Calendar{ID: id, ProviderCalendarID: raw, ConnectionID: "imported", Name: "Synthetic", Timezone: "UTC", CanRead: true, DiscoveredAt: f.now}); err != nil {
		t.Fatal(err)
	}
	page, _, err := f.svc.ReadCoverage(f.ctx, f.page, "")
	if err != nil || len(page.Sources) != 16 {
		t.Fatalf("default imported exclusion: %v sources=%d", err, len(page.Sources))
	}
	f.svc.includeImportedCalendars = true
	if _, _, err = f.svc.ReadCoverage(f.ctx, f.page, ""); !errors.Is(err, storage.ErrExplicitCachedPageScope) {
		t.Fatalf("included imported calendar did not count: %v", err)
	}
	request := f.page
	request.CalendarIDs = []string{id}
	f.svc.includeImportedCalendars = false
	page, _, err = f.svc.ReadCoverage(f.ctx, request, "")
	if err != nil || len(page.Sources) != 1 {
		t.Fatalf("explicit imported bypass: %v %+v", err, page)
	}
}

func TestCoverageExpiryDuringFinalValidation(t *testing.T) {
	f := newCoverageFixture(t, 2)
	now := f.now
	f.svc.now = func() time.Time { return now }
	f.svc.testBeforeFinalValidation = func() { now = now.Add(cursorLifetime + time.Second) }
	page, _, err := f.svc.ReadCoverage(f.ctx, f.page, "")
	if !errors.Is(err, ErrUnavailable) || len(page.Items) != 0 {
		t.Fatalf("expired page released: %v %+v", err, page)
	}
}
