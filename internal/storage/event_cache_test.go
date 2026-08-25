package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
)

func TestRawEventArtifactIsEncryptedAndLinkedToQuarantine(t *testing.T) {
	store, now := newEventCacheStore(t)
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store.SetArtifactCipher(cipher)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("BEGIN:VCALENDAR\r\nBROKEN\r\nEND:VCALENDAR\r\n")
	if err := upsertEventSyncWarning(context.Background(), store, tx, "calendar", EventSyncWarning{
		ObjectID: "broken.ics", ETag: "etag-1", ErrorCode: "protocol",
		Diagnostic: &calendar.EventSyncDiagnostic{ContentType: "text/calendar", RawPayload: raw},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	if err := store.db.QueryRow("SELECT payload_ciphertext FROM calendar_sync_raw_artifacts WHERE calendar_id=? AND object_id=?", "calendar", "broken.ics").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, raw) {
		t.Fatal("raw payload persisted in plaintext")
	}
	decoded, err := cipher.Decrypt(encrypted, []byte("calendar-sync-artifact\x00calendar\x00broken.ics\x00etag-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatalf("decrypted payload = %q, want %q", decoded, raw)
	}
}

func TestMigrateUpgradesVersionOneToEventReadModel(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar-v1.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	script, err := migrations.ReadFile("migrations/sqlite/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, string(script)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES (1, ?)", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, store, SchemaVersion)
	for _, table := range []string{"cached_events", "calendar_sync_state", "calendar_sync_objects"} {
		var name string
		if err := store.db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			t.Fatalf("table %q: %v", table, err)
		}
	}
	var authorizationColumn int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('calendar_sync_quarantine') WHERE name='provider_mutation_authorized_etag'`).Scan(&authorizationColumn); err != nil || authorizationColumn != 1 {
		t.Fatalf("repair authorization migration column=%d err=%v", authorizationColumn, err)
	}
}

func TestMigrateFreshAndIdempotentEventReadModel(t *testing.T) {
	store := newSQLiteStore(t)
	assertSchemaVersion(t, store, SchemaVersion)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, store, SchemaVersion)
}

func TestOperatorRepairAuthorizationIsETagBoundAndSingleUse(t *testing.T) {
	store, now := newEventCacheStore(t)
	state, err := store.ClaimDueCalendarSync(t.Context(), "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	next := now.Add(time.Hour)
	if err := store.ApplyEventSyncPage(t.Context(), *state, EventSyncBatch{Warnings: []EventSyncWarning{{ObjectID: "broken", ETag: "etag-1", ErrorCode: "protocol"}}, NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ScheduleEventSyncObjectRepair(t.Context(), "calendar", "broken", "stale-etag", now.Add(time.Minute)); !errors.Is(err, ErrEventSyncRepairETagMismatch) {
		t.Fatalf("stale authorization error=%v", err)
	}
	var grant string
	if err := store.db.QueryRowContext(t.Context(), "SELECT provider_mutation_authorized_etag FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=?", "calendar", "broken").Scan(&grant); err != nil || grant != "" {
		t.Fatalf("stale authorization grant=%q err=%v", grant, err)
	}
	if _, err := store.ScheduleEventSyncObjectRepair(t.Context(), "calendar", "broken", "etag-1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, err = store.ClaimDueCalendarSync(t.Context(), "worker", now.Add(time.Minute), now.Add(2*time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim authorized=%#v err=%v", state, err)
	}
	due, err := store.ListDueEventSyncQuarantine(t.Context(), *state, now.Add(time.Minute), 1)
	if err != nil || len(due) != 1 || due[0].ProviderMutationAuthorizedETag != "etag-1" {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	consumed, err := store.ConsumeEventSyncProviderMutationAuthorization(t.Context(), *state, "calendar", "broken", "etag-1", now.Add(time.Minute))
	if err != nil || !consumed {
		t.Fatalf("consume=%v err=%v", consumed, err)
	}
	consumed, err = store.ConsumeEventSyncProviderMutationAuthorization(t.Context(), *state, "calendar", "broken", "etag-1", now.Add(time.Minute))
	if err != nil || consumed {
		t.Fatalf("second consume=%v err=%v", consumed, err)
	}
}

func TestProviderCorrectionIsAuditedAfterResolvingQuarantine(t *testing.T) {
	store, now := newEventCacheStore(t)
	state, err := store.ClaimDueCalendarSync(t.Context(), "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	next := now.Add(time.Hour)
	if err := store.ApplyEventSyncPage(t.Context(), *state, EventSyncBatch{Warnings: []EventSyncWarning{{ObjectID: "all-day", ETag: "before", ErrorCode: "protocol"}}, NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	state, err = store.ClaimDueCalendarSync(t.Context(), "worker", next, next.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("repair claim=%#v err=%v", state, err)
	}
	correctedAt := next.Add(time.Minute)
	upsert := CachedEventUpsert{SourceObjectID: "all-day", Event: cachedAllDayEvent("all-day", "2026-08-20", "2026-08-21")}
	if err := store.ApplyEventSyncObjectRepair(t.Context(), *state, EventSyncRepairBatch{ObjectID: "all-day", ETag: "after", Outcome: calendar.EventSyncObjectProviderCorrected, Upserts: []CachedEventUpsert{upsert}}, correctedAt); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := store.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM calendar_sync_quarantine WHERE calendar_id=? AND object_id=? AND active=?", "calendar", "all-day", true).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active quarantine=%d err=%v", active, err)
	}
	corrections, err := store.ListRecentEventSyncProviderCorrections(t.Context(), 10)
	if err != nil || len(corrections) != 1 {
		t.Fatalf("corrections=%#v err=%v", corrections, err)
	}
	got := corrections[0]
	if got.CalendarID != "calendar" || got.ObjectID != "all-day" || got.Outcome != string(calendar.EventSyncObjectProviderCorrected) || !got.CorrectedAt.Equal(correctedAt) || got.Provider != "google" {
		t.Fatalf("correction=%#v", got)
	}
}

func TestPostgresEventReadModelIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	store, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	connectionID := "event-read-model-test-" + suffix
	calendarID := "event-read-model-calendar-" + suffix
	now := time.Now().UTC()
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), store.query("DELETE FROM connections WHERE id=?"), connectionID)
	})
	if err := store.CreateConnection(t.Context(), Connection{ID: connectionID, Provider: "google", DisplayName: "Integration", Status: "connected", EncryptedCredentials: []byte("test"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(t.Context(), Calendar{ID: calendarID, ConnectionID: connectionID, ProviderCalendarID: "primary", Name: "Integration", CanRead: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	window := SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	if err := store.EnsureCalendarSyncStates(t.Context(), now, window); err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "event", CalendarID: calendarID, Provider: "google", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(30 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	if err := store.UpsertCachedEvent(t.Context(), event, now); err != nil {
		t.Fatal(err)
	}
	events, _, err := store.ListCachedEvents(t.Context(), []string{calendarID}, window.Start, window.End)
	if err != nil || len(events) != 1 || events[0].ID != event.ID {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestCachedEventsOverlapReplayAndTombstones(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	timed := cachedTimedEvent("timed", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")
	allDay := cachedAllDayEvent("all-day", "2026-08-22", "2026-08-24")
	if err := store.UpsertCachedEvent(ctx, timed, now); err != nil {
		t.Fatal(err)
	}
	timed.Title = "replayed replacement"
	if err := store.UpsertCachedEvent(ctx, timed, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCachedEvent(ctx, allDay, now); err != nil {
		t.Fatal(err)
	}
	events, sources, err := store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-22T10:30:00Z"), mustTime(t, "2026-08-22T10:45:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	var replayed *calendar.EventV2
	for index := range events {
		if events[index].ID == "timed" {
			replayed = &events[index]
		}
	}
	if len(events) != 2 || replayed == nil || replayed.Title != "replayed replacement" || len(sources) != 1 || sources[0].Status != "pending" {
		t.Fatalf("events=%#v sources=%#v", events, sources)
	}
	if err := store.DeleteCachedEvent(ctx, calendar.EventRef{CalendarID: "calendar", EventID: "timed"}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	events, _, err = store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-22T09:00:00Z"), mustTime(t, "2026-08-22T12:00:00Z"))
	if err != nil || len(events) != 1 || events[0].ID != "all-day" {
		t.Fatalf("events after tombstone=%#v err=%v", events, err)
	}
}

func TestListCachedEventsReturnsExpandedRecurrenceOnly(t *testing.T) {
	store, now := newEventCacheStore(t)
	master := cachedTimedEvent("series", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")
	master.InstanceKind = "seriesMaster"
	occurrence := cachedTimedEvent("series#instance", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")
	occurrence.InstanceKind = "occurrence"
	occurrence.RecurringEventID = "series"
	for _, event := range []calendar.EventV2{master, occurrence} {
		if err := store.UpsertCachedEvent(t.Context(), event, now); err != nil {
			t.Fatal(err)
		}
	}
	events, _, err := store.ListCachedEvents(t.Context(), []string{"calendar"}, mustTime(t, "2026-08-22T09:00:00Z"), mustTime(t, "2026-08-22T12:00:00Z"))
	if err != nil || len(events) != 1 || events[0].ID != occurrence.ID {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestListCachedEventsExcludesDisconnectedAndUnreadableCalendars(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	event := cachedTimedEvent("visible-when-healthy", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")
	if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
		t.Fatal(err)
	}
	list := func() ([]calendar.EventV2, []CachedSourceStatus, error) {
		return store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-22T09:00:00Z"), mustTime(t, "2026-08-22T12:00:00Z"))
	}
	events, statuses, err := list()
	if err != nil || len(events) != 1 || len(statuses) != 1 {
		t.Fatalf("healthy read events=%#v statuses=%#v err=%v", events, statuses, err)
	}
	if err := store.UpdateConnectionVerification(ctx, "connection", "pending", "disconnected", now); err != nil {
		t.Fatal(err)
	}
	events, statuses, err = list()
	if err != nil || len(events) != 0 || len(statuses) != 0 {
		t.Fatalf("disconnected read events=%#v statuses=%#v err=%v", events, statuses, err)
	}
	if err := store.UpdateConnectionVerification(ctx, "connection", "connected", "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE calendars SET can_read=? WHERE id='calendar'", false); err != nil {
		t.Fatal(err)
	}
	events, statuses, err = list()
	if err != nil || len(events) != 0 || len(statuses) != 0 {
		t.Fatalf("unreadable read events=%#v statuses=%#v err=%v", events, statuses, err)
	}
}

func TestApplyEventSyncPageAdvancesCursorAndSweepsOnlyCompletedFullGeneration(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if err := store.UpsertCachedEvent(ctx, cachedTimedEvent("old", "2026-08-20T10:00:00Z", "2026-08-20T11:00:00Z"), now); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	page := EventSyncBatch{FullSync: true, Upserts: []CachedEventUpsert{{Event: cachedTimedEvent("current", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")}}, NextCursor: "cursor-2"}
	page.Objects = []SyncObject{{ObjectID: "old-resource", ETag: "old"}}
	if err := store.ApplyEventSyncPage(ctx, *state, page, false, now); err != nil {
		t.Fatal(err)
	}
	page.Objects = []SyncObject{{ObjectID: "current-resource", ETag: "new"}}
	if err := store.ApplyEventSyncPage(ctx, *state, page, true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&state.Cursor, &state.Status); err != nil {
		t.Fatal(err)
	}
	if state.Cursor != "cursor-2" || state.Status != "ready" {
		t.Fatalf("state after final=%#v", state)
	}
	events, _, err := store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-20T00:00:00Z"), mustTime(t, "2026-08-23T00:00:00Z"))
	if err != nil || len(events) != 1 || events[0].ID != "current" {
		t.Fatalf("events after sweep=%#v err=%v", events, err)
	}
	var objects int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM calendar_sync_objects WHERE calendar_id='calendar' AND object_id='current-resource'").Scan(&objects); err != nil || objects != 1 {
		t.Fatalf("object inventory count=%d err=%v", objects, err)
	}
}

func TestApplyEventSyncPageDegradedFinalAdvancesCursorAndRetries(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	initial, err := store.ClaimDueCalendarSync(ctx, "snapshot-worker", now, now.Add(time.Hour))
	if err != nil || initial == nil {
		t.Fatalf("initial claim=%#v err=%v", initial, err)
	}
	nextDue := now.Add(time.Hour)
	snapshot := EventSyncBatch{
		FullSync: true,
		Upserts: []CachedEventUpsert{
			{SourceObjectID: "valid-a.ics", Event: cachedTimedEvent("valid-a", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")},
			{SourceObjectID: "valid-b.ics", Event: cachedTimedEvent("valid-b", "2026-08-22T11:00:00Z", "2026-08-22T12:00:00Z")},
			{SourceObjectID: "unresolved.ics", Event: cachedTimedEvent("unresolved", "2026-08-22T12:00:00Z", "2026-08-22T13:00:00Z")},
		},
		Objects: []SyncObject{
			{ObjectID: "valid-a.ics", ETag: "v1"},
			{ObjectID: "valid-b.ics", ETag: "v1"},
			{ObjectID: "unresolved.ics", ETag: "v1"},
		},
		NextCursor: "authoritative-cursor",
		NextSyncAt: &nextDue,
	}
	if err := store.ApplyEventSyncPage(ctx, *initial, snapshot, true, now); err != nil {
		t.Fatal(err)
	}
	var unresolvedGeneration int64
	if err := store.db.QueryRowContext(ctx, "SELECT sync_generation FROM calendar_sync_objects WHERE calendar_id='calendar' AND object_id='unresolved.ics'").Scan(&unresolvedGeneration); err != nil {
		t.Fatal(err)
	}

	degradedAt := nextDue
	claim, err := store.ClaimDueCalendarSync(ctx, "degraded-worker", degradedAt, degradedAt.Add(time.Hour))
	if err != nil || claim == nil {
		t.Fatalf("degraded claim=%#v err=%v", claim, err)
	}
	retryAt := degradedAt.Add(5 * time.Minute)
	degraded := EventSyncBatch{
		FullSync:          true,
		Degraded:          true,
		ErrorCode:         "protocol",
		Warnings:          []EventSyncWarning{{ObjectID: "unresolved.ics", ErrorCode: "protocol", ETag: "etag-1"}},
		ReplacedObjectIDs: []string{"valid-a.ics"},
		Upserts: []CachedEventUpsert{{
			SourceObjectID: "valid-a.ics",
			Event:          cachedTimedEvent("valid-a", "2026-08-22T10:00:00Z", "2026-08-22T11:30:00Z"),
		}},
		NextCursor: "degraded-cursor",
		NextSyncAt: &retryAt,
	}
	if err := store.ApplyEventSyncPage(ctx, *claim, degraded, true, degradedAt); err != nil {
		t.Fatal(err)
	}

	var cursor, status, code string
	var generation int64
	var success time.Time
	var next time.Time
	var leaseOwner sql.NullString
	var leaseUntil sql.NullTime
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status, generation, last_success_at, last_error_code, next_sync_at, lease_owner, lease_until FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor, &status, &generation, &success, &code, &next, &leaseOwner, &leaseUntil); err != nil {
		t.Fatal(err)
	}
	if cursor != degraded.NextCursor || generation != claim.Generation || !success.Equal(degradedAt) || status != "degraded" || code != "protocol" || !next.Equal(retryAt) || leaseOwner.Valid || leaseUntil.Valid {
		t.Fatalf("degraded final state cursor=%q generation=%d success=%s status=%q code=%q next=%s lease=%q/%v", cursor, generation, success, status, code, next, leaseOwner.String, leaseUntil)
	}
	var afterUnresolvedGeneration int64
	if err := store.db.QueryRowContext(ctx, "SELECT sync_generation FROM calendar_sync_objects WHERE calendar_id='calendar' AND object_id='unresolved.ics'").Scan(&afterUnresolvedGeneration); err != nil {
		t.Fatal(err)
	}
	if afterUnresolvedGeneration != unresolvedGeneration {
		t.Fatalf("unresolved object generation=%d, want %d", afterUnresolvedGeneration, unresolvedGeneration)
	}
	events, _, err := store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-22T09:00:00Z"), mustTime(t, "2026-08-22T14:00:00Z"))
	if err != nil || len(events) != 2 {
		t.Fatalf("degraded events=%#v err=%v", events, err)
	}
	ids := make(map[string]calendar.EventV2, len(events))
	for _, event := range events {
		ids[event.ID] = event
	}
	if ids["valid-a"].End.DateTime != "2026-08-22T11:30:00Z" || ids["valid-b"].ID != "" || ids["unresolved"].ID == "" {
		t.Fatalf("degraded projection=%#v", ids)
	}
	cleanClaim, err := store.ClaimDueCalendarSync(ctx, "clean-worker", retryAt, retryAt.Add(time.Hour))
	if err != nil || cleanClaim == nil {
		t.Fatalf("clean claim=%#v err=%v", cleanClaim, err)
	}
	cleanNext := retryAt.Add(time.Minute)
	if err := store.ApplyEventSyncPage(ctx, *cleanClaim, EventSyncBatch{NextCursor: "clean-cursor", NextSyncAt: &cleanNext}, true, retryAt); err != nil {
		t.Fatal(err)
	}
	var cleanStatus string
	if err := store.db.QueryRowContext(ctx, "SELECT status FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cleanStatus); err != nil {
		t.Fatal(err)
	}
	if cleanStatus != "degraded" {
		t.Fatalf("clean sync status=%q, want degraded while quarantine is active", cleanStatus)
	}
}

func TestDegradedCalendarSyncIsClaimedOnlyWhenDue(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	due := now.Add(5 * time.Minute)
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET status='degraded', next_sync_at=?, lease_owner=NULL, lease_until=NULL WHERE calendar_id='calendar'", due); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueCalendarSync(ctx, "worker", due.Add(-time.Second), due.Add(time.Hour))
	if err != nil || claimed != nil {
		t.Fatalf("early degraded claim=%#v err=%v", claimed, err)
	}
	claimed, err = store.ClaimDueCalendarSync(ctx, "worker", due, due.Add(time.Hour))
	if err != nil || claimed == nil || claimed.Status != "syncing" {
		t.Fatalf("due degraded claim=%#v err=%v", claimed, err)
	}
}

func TestNonFinalDegradedPageDoesNotReleaseLease(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	retryAt := now.Add(5 * time.Minute)
	batch := EventSyncBatch{
		Degraded:  true,
		ErrorCode: "protocol",
		Upserts: []CachedEventUpsert{{
			Event: cachedTimedEvent("valid-page-row", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z"),
		}},
		NextCursor: "must-not-advance",
		NextSyncAt: &retryAt,
	}
	if err := store.ApplyEventSyncPage(ctx, *state, batch, false, now); err != nil {
		t.Fatal(err)
	}
	var cursor, status, owner string
	var next time.Time
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status, lease_owner, next_sync_at FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor, &status, &owner, &next); err != nil {
		t.Fatal(err)
	}
	if cursor != "" || status != "syncing" || owner != "worker" || !next.Equal(now) {
		t.Fatalf("non-final degraded state cursor=%q status=%q owner=%q next=%s", cursor, status, owner, next)
	}
}

func TestStaleWorkerCannotApplyDegradedFinal(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	stale, err := store.ClaimDueCalendarSync(ctx, "stale-worker", now, now.Add(time.Minute))
	if err != nil || stale == nil {
		t.Fatalf("stale claim=%#v err=%v", stale, err)
	}
	reclaimedAt := now.Add(time.Minute)
	active, err := store.ClaimDueCalendarSync(ctx, "active-worker", reclaimedAt, reclaimedAt.Add(time.Hour))
	if err != nil || active == nil {
		t.Fatalf("reclaimed claim=%#v err=%v", active, err)
	}
	retryAt := reclaimedAt.Add(5 * time.Minute)
	batch := EventSyncBatch{
		Degraded:  true,
		ErrorCode: "protocol",
		Upserts: []CachedEventUpsert{{
			Event: cachedTimedEvent("must-not-write", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z"),
		}},
		NextSyncAt: &retryAt,
	}
	if err := store.ApplyEventSyncPage(ctx, *stale, batch, true, reclaimedAt); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale degraded final error=%v", err)
	}
	var cursor, status, owner string
	var generation int64
	var events int
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status, lease_owner, generation FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor, &status, &owner, &generation); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cached_events WHERE calendar_id='calendar' AND event_id='must-not-write'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if cursor != "authoritative" || status != "syncing" || owner != "active-worker" || generation != active.Generation || events != 0 {
		t.Fatalf("stale degraded mutation cursor=%q status=%q owner=%q generation=%d events=%d", cursor, status, owner, generation, events)
	}
}

func TestCalendarSyncLeasesRecoverAndFailuresPreserveCursor(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimDueCalendarSync(ctx, "first", now, now.Add(time.Minute))
	if err != nil || first == nil {
		t.Fatalf("first claim=%#v err=%v", first, err)
	}
	second, err := store.ClaimDueCalendarSync(ctx, "second", now.Add(30*time.Second), now.Add(2*time.Minute))
	if err != nil || second != nil {
		t.Fatalf("overlapping claim=%#v err=%v", second, err)
	}
	second, err = store.ClaimDueCalendarSync(ctx, "second", now.Add(time.Minute), now.Add(2*time.Minute))
	if err != nil || second == nil || second.Cursor != "authoritative" {
		t.Fatalf("stale recovery=%#v err=%v", second, err)
	}
	if err := store.FailCalendarSync(ctx, *second, "provider_down", now.Add(time.Minute), now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var cursor, status, code string
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status, last_error_code FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor, &status, &code); err != nil {
		t.Fatal(err)
	}
	if cursor != "authoritative" || status != "failed" || code != "provider_down" {
		t.Fatalf("failed state cursor=%q status=%q code=%q", cursor, status, code)
	}
	_, statuses, err := store.ListCachedEvents(ctx, []string{"calendar"}, now, now.Add(time.Hour))
	if err != nil || len(statuses) != 1 || statuses[0].Status != "failed" || statuses[0].ErrorCode != "provider_down" {
		t.Fatalf("statuses=%#v err=%v", statuses, err)
	}
}

func TestResetCalendarSyncClearsCursorAndInvalidatesOldClaim(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='expired-cursor', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	reset, err := store.ResetCalendarSync(ctx, *state, now)
	if err != nil || reset == nil || reset.Cursor != "" || reset.Generation != state.Generation+1 {
		t.Fatalf("reset=%#v err=%v", reset, err)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{FullSync: true, NextCursor: "stale"}, true, now); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("old claim apply error=%v", err)
	}
}

func TestExpiredCalendarSyncLeaseCannotApplyFailOrReset(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	expiredNow := now.Add(time.Minute)
	stalePage := EventSyncBatch{Upserts: []CachedEventUpsert{{Event: cachedTimedEvent("must-not-write", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")}}, NextCursor: "must-not-advance"}
	if err := store.ApplyEventSyncPage(ctx, *state, stalePage, true, expiredNow); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale apply error=%v", err)
	}
	if err := store.FailCalendarSync(ctx, *state, "stale", expiredNow, expiredNow.Add(time.Hour)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale fail error=%v", err)
	}
	if _, err := store.ResetCalendarSync(ctx, *state, expiredNow); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale reset error=%v", err)
	}
	var cursor string
	var events int
	if err := store.db.QueryRowContext(ctx, "SELECT cursor FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cached_events WHERE calendar_id='calendar'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if cursor != "authoritative" || events != 0 {
		t.Fatalf("stale mutations cursor=%q events=%d", cursor, events)
	}
}

func TestObjectReplacementAndDeletionTombstoneAllMembers(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	first := EventSyncBatch{
		Upserts: []CachedEventUpsert{
			{SourceObjectID: "resource-1.ics", Event: cachedTimedEvent("member-a", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")},
			{SourceObjectID: "resource-1.ics", Event: cachedTimedEvent("member-b", "2026-08-22T11:00:00Z", "2026-08-22T12:00:00Z")},
		},
		Objects: []SyncObject{{ObjectID: "resource-1.ics", ETag: "v1"}},
	}
	if err := store.ApplyEventSyncPage(ctx, *state, first, false, now); err != nil {
		t.Fatal(err)
	}
	replacement := EventSyncBatch{
		Upserts:           []CachedEventUpsert{{SourceObjectID: "resource-1.ics", Event: cachedTimedEvent("member-a", "2026-08-22T10:00:00Z", "2026-08-22T11:30:00Z")}},
		ReplacedObjectIDs: []string{"resource-1.ics"},
		Objects:           []SyncObject{{ObjectID: "resource-1.ics", ETag: "v2"}},
	}
	if err := store.ApplyEventSyncPage(ctx, *state, replacement, false, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	events, _, err := store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-22T09:00:00Z"), mustTime(t, "2026-08-22T13:00:00Z"))
	if err != nil || len(events) != 1 || events[0].ID != "member-a" {
		t.Fatalf("events after replacement=%#v err=%v", events, err)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{DeletedObjectIDs: []string{"resource-1.ics"}}, false, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	events, _, err = store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-22T09:00:00Z"), mustTime(t, "2026-08-22T13:00:00Z"))
	if err != nil || len(events) != 0 {
		t.Fatalf("events after object deletion=%#v err=%v", events, err)
	}
	var inventory, memberBDeleted int
	var sourceObjectID string
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM calendar_sync_objects WHERE calendar_id='calendar' AND object_id='resource-1.ics'").Scan(&inventory); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT deleted FROM cached_events WHERE calendar_id='calendar' AND event_id='member-b'").Scan(&memberBDeleted); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT source_object_id FROM cached_events WHERE calendar_id='calendar' AND event_id='member-a'").Scan(&sourceObjectID); err != nil {
		t.Fatal(err)
	}
	if inventory != 0 || memberBDeleted != 1 || sourceObjectID != "resource-1.ics" {
		t.Fatalf("inventory=%d memberBDeleted=%d sourceObjectID=%q", inventory, memberBDeleted, sourceObjectID)
	}
}

func TestPartialObjectPagesDoNotTombstoneMembersWithoutReplacementSignal(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	first := EventSyncBatch{Upserts: []CachedEventUpsert{{SourceObjectID: "resource-2.ics", Event: cachedTimedEvent("first", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")}}}
	second := EventSyncBatch{Upserts: []CachedEventUpsert{{SourceObjectID: "resource-2.ics", Event: cachedTimedEvent("second", "2026-08-22T11:00:00Z", "2026-08-22T12:00:00Z")}}}
	if err := store.ApplyEventSyncPage(ctx, *state, first, false, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, second, false, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	events, _, err := store.ListCachedEvents(ctx, []string{"calendar"}, mustTime(t, "2026-08-22T09:00:00Z"), mustTime(t, "2026-08-22T13:00:00Z"))
	if err != nil || len(events) != 2 {
		t.Fatalf("partial page events=%#v err=%v", events, err)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{ReplacedObjectIDs: []string{"resource-2.ics"}}, false, now.Add(2*time.Minute)); err == nil {
		t.Fatal("empty replacement membership succeeded")
	}
}

func TestEnsureCalendarSyncStatesResetsUnleasedChangedWindow(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	window := SyncWindow{Start: now.AddDate(-2, 0, 0), End: now.AddDate(3, 0, 0)}
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative' WHERE calendar_id='calendar'"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	var cursor, status string
	var generation int64
	var start, end time.Time
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status, generation, window_start, window_end FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor, &status, &generation, &start, &end); err != nil {
		t.Fatal(err)
	}
	if cursor != "" || status != "pending" || generation != 1 || !start.Equal(window.Start) || !end.Equal(window.End) {
		t.Fatalf("window reset cursor=%q status=%q generation=%d window=%s--%s", cursor, status, generation, start, end)
	}
}

func TestEnsureCalendarSyncStatesFreezesLeasedWindowAndInvalidatesExpiredClaim(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	oldWindow := SyncWindow{Start: now.AddDate(-1, 0, 0), End: now.AddDate(2, 0, 0)}
	newWindow := SyncWindow{Start: now.AddDate(-2, 0, 0), End: now.AddDate(3, 0, 0)}
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now.Add(time.Second), oldWindow); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now.Add(10*time.Second), newWindow); err != nil {
		t.Fatal(err)
	}
	var start, end time.Time
	if err := store.db.QueryRowContext(ctx, "SELECT window_start, window_end FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&start, &end); err != nil {
		t.Fatal(err)
	}
	if !start.Equal(oldWindow.Start) || !end.Equal(oldWindow.End) {
		t.Fatalf("leased window changed to %s--%s", start, end)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now.Add(time.Minute), newWindow); err != nil {
		t.Fatal(err)
	}
	var cursor, status string
	var generation int64
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status, generation, window_start, window_end FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor, &status, &generation, &start, &end); err != nil {
		t.Fatal(err)
	}
	if cursor != "" || status != "pending" || generation != state.Generation+1 || !start.Equal(newWindow.Start) || !end.Equal(newWindow.End) {
		t.Fatalf("reset state cursor=%q status=%q generation=%d window=%s--%s", cursor, status, generation, start, end)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{NextCursor: "stale"}, true, now.Add(time.Minute)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("old claim finalized after window reset: %v", err)
	}
}

func TestParkedCalendarSyncIsNotClaimedAndCanBeReactivated(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	if err := store.ParkCalendarSync(ctx, *state, "credentials_invalid", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueCalendarSync(ctx, "other", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil || claimed != nil {
		t.Fatalf("parked claim=%#v err=%v", claimed, err)
	}
	if err := store.ScheduleCalendarSync(ctx, "calendar", now.Add(2*time.Minute), false); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimDueCalendarSync(ctx, "other", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil || claimed == nil || claimed.Cursor != "authoritative" {
		t.Fatalf("reactivated claim=%#v err=%v", claimed, err)
	}
}

func TestRetryableFailedCalendarSyncIsClaimedWhenDue(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	next := now.Add(5 * time.Minute)
	if err := store.FailCalendarSync(ctx, *state, "temporary_provider_error", now.Add(time.Minute), next); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueCalendarSync(ctx, "retry", next, next.Add(time.Hour))
	if err != nil || claimed == nil || claimed.Status != "syncing" {
		t.Fatalf("retry claim=%#v err=%v", claimed, err)
	}
}

func TestScheduleCalendarSyncCannotInvalidateLiveLease(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	if err := store.ScheduleCalendarSync(ctx, "calendar", now.Add(time.Minute), true); !errors.Is(err, ErrCalendarSyncActive) {
		t.Fatalf("live lease schedule error=%v", err)
	}
	var cursor, status string
	var generation int64
	if err := store.db.QueryRowContext(ctx, "SELECT cursor, status, generation FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&cursor, &status, &generation); err != nil {
		t.Fatal(err)
	}
	if cursor != "authoritative" || status != "syncing" || generation != state.Generation {
		t.Fatalf("live lease state cursor=%q status=%q generation=%d", cursor, status, generation)
	}
}

func TestEnsureAndClaimOnlyConnectedReadableCalendars(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if err := store.CreateConnection(ctx, Connection{ID: "pending-connection", Provider: "apple", DisplayName: "Apple", Status: "pending", EncryptedCredentials: []byte("cipher"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(ctx, Calendar{ID: "pending-calendar", ConnectionID: "pending-connection", ProviderCalendarID: "primary", Name: "Pending", CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	window := SyncWindow{Start: now.AddDate(-1, 0, 0), End: now.AddDate(2, 0, 0)}
	if err := store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	var pendingStates int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM calendar_sync_state WHERE calendar_id='pending-calendar'").Scan(&pendingStates); err != nil {
		t.Fatal(err)
	}
	if pendingStates != 0 {
		t.Fatalf("pending connection state count=%d", pendingStates)
	}
	if err := store.UpdateConnectionVerification(ctx, "connection", "pending", "disconnected", now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || claimed != nil {
		t.Fatalf("disconnected claim=%#v err=%v", claimed, err)
	}
}

func TestClaimDueCalendarSyncRejectsStateAfterCalendarBecomesUnreadable(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendars SET can_read=? WHERE id='calendar'", false); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || claimed != nil {
		t.Fatalf("unreadable claim=%#v err=%v", claimed, err)
	}
}

func TestScheduleCalendarSyncRequiresEligibleCalendar(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown", func(t *testing.T) {
		store, now := newEventCacheStore(t)
		if err := store.ScheduleCalendarSync(ctx, "missing", now, false); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown schedule error=%v", err)
		}
	})
	t.Run("disconnected", func(t *testing.T) {
		store, now := newEventCacheStore(t)
		if err := store.UpdateConnectionVerification(ctx, "connection", "pending", "disconnected", now); err != nil {
			t.Fatal(err)
		}
		if err := store.ScheduleCalendarSync(ctx, "calendar", now, false); !errors.Is(err, ErrCalendarSyncIneligible) {
			t.Fatalf("disconnected schedule error=%v", err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		store, now := newEventCacheStore(t)
		if _, err := store.db.ExecContext(ctx, "UPDATE calendars SET can_read=? WHERE id='calendar'", false); err != nil {
			t.Fatal(err)
		}
		if err := store.ScheduleCalendarSync(ctx, "calendar", now, false); !errors.Is(err, ErrCalendarSyncIneligible) {
			t.Fatalf("unreadable schedule error=%v", err)
		}
	})
	t.Run("eligible", func(t *testing.T) {
		store, now := newEventCacheStore(t)
		if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET status='parked', next_sync_at=? WHERE calendar_id='calendar'", now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := store.ScheduleCalendarSync(ctx, "calendar", now, false); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
		if err != nil || claimed == nil {
			t.Fatalf("eligible claim=%#v err=%v", claimed, err)
		}
	})
}

func TestRenewCalendarSyncLeaseRejectsExpiredAndReclaimedClaims(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	renewedUntil := now.Add(2 * time.Minute)
	if err := store.RenewCalendarSyncLease(ctx, *state, now.Add(30*time.Second), renewedUntil); err != nil {
		t.Fatal(err)
	}
	var status string
	var cursor string
	if err := store.db.QueryRowContext(ctx, "SELECT status, cursor FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&status, &cursor); err != nil {
		t.Fatal(err)
	}
	if status != "syncing" || cursor != "" {
		t.Fatalf("renewed state status=%q cursor=%q", status, cursor)
	}
	if err := store.RenewCalendarSyncLease(ctx, *state, now.Add(30*time.Second), now.Add(90*time.Second)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("shorter renewal error=%v", err)
	}
	reclaimed, err := store.ClaimDueCalendarSync(ctx, "new-worker", renewedUntil, renewedUntil.Add(time.Hour))
	if err != nil || reclaimed == nil || reclaimed.LeaseOwner != "new-worker" {
		t.Fatalf("reclaim=%#v err=%v", reclaimed, err)
	}
	if err := store.RenewCalendarSyncLease(ctx, *state, renewedUntil, renewedUntil.Add(2*time.Hour)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("reclaimed renewal error=%v", err)
	}

	expiredStore, expiredNow := newEventCacheStore(t)
	expired, err := expiredStore.ClaimDueCalendarSync(ctx, "worker", expiredNow, expiredNow.Add(time.Minute))
	if err != nil || expired == nil {
		t.Fatalf("expired claim=%#v err=%v", expired, err)
	}
	if err := expiredStore.RenewCalendarSyncLease(ctx, *expired, expiredNow.Add(time.Minute), expiredNow.Add(2*time.Minute)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("expired renewal error=%v", err)
	}
}

func TestConnectedVerificationReactivatesOnlyParkedCalendarSync(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "UPDATE calendar_sync_state SET cursor='authoritative', next_sync_at=? WHERE calendar_id='calendar'", now); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	if err := store.ParkCalendarSync(ctx, *state, "credentials_invalid", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	verifiedAt := now.Add(2 * time.Minute)
	if err := store.UpdateConnectionVerification(ctx, "connection", "connected", "", verifiedAt); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueCalendarSync(ctx, "next-worker", verifiedAt, verifiedAt.Add(time.Hour))
	if err != nil || claimed == nil || claimed.Cursor != "authoritative" {
		t.Fatalf("connected reactivation=%#v err=%v", claimed, err)
	}

	secondStore, secondNow := newEventCacheStore(t)
	secondState, err := secondStore.ClaimDueCalendarSync(ctx, "worker", secondNow, secondNow.Add(time.Hour))
	if err != nil || secondState == nil {
		t.Fatalf("second claim=%#v err=%v", secondState, err)
	}
	if err := secondStore.ParkCalendarSync(ctx, *secondState, "credentials_invalid", secondNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := secondStore.UpdateConnectionVerification(ctx, "connection", "pending", "disconnected", secondNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := secondStore.db.QueryRowContext(ctx, "SELECT status FROM calendar_sync_state WHERE calendar_id='calendar'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "parked" {
		t.Fatalf("non-connected verification reactivated status=%q", status)
	}
}

func newEventCacheStore(t *testing.T) (*Store, time.Time) {
	t.Helper()
	store := newSQLiteStore(t)
	now := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if err := store.CreateConnection(ctx, Connection{ID: "connection", Provider: "google", DisplayName: "Google", Status: "connected", EncryptedCredentials: []byte("cipher"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(ctx, Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary", CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now.AddDate(-1, 0, 0), End: now.AddDate(2, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	return store, now
}

func cachedTimedEvent(id, start, end string) calendar.EventV2 {
	return calendar.EventV2{ID: id, CalendarID: "calendar", Provider: "google", ETag: "etag-" + id, Start: calendar.EventTime{DateTime: start, TimeZone: "UTC"}, End: calendar.EventTime{DateTime: end, TimeZone: "UTC"}}
}

func cachedAllDayEvent(id, start, end string) calendar.EventV2 {
	return calendar.EventV2{ID: id, CalendarID: "calendar", Provider: "google", ETag: "etag-" + id, Start: calendar.EventTime{Date: start}, End: calendar.EventTime{Date: end}}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func assertSchemaVersion(t *testing.T, store *Store, want int) {
	t.Helper()
	var got int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("schema version = %d, want %d", got, want)
	}
}
