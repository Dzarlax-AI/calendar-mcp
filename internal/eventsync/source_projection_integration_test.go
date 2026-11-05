package eventsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func TestBackgroundSyncPublishesCacheCopySource(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	runBackgroundSyncPublishesCacheCopySource(t, s)
}

func runBackgroundSyncPublishesCacheCopySource(t *testing.T, s *storage.Store) {
	t.Helper()
	ctx := context.Background()
	var err error
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if err = s.CreateConnection(ctx, storage.Connection{ID: "connection", Provider: "google", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"google:source"} {
		if err = s.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: "connection", ProviderCalendarID: id, Name: id, Timezone: "UTC", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	window := storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	if err = s.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		t.Fatal(err)
	}
	initial, err := s.ClaimDueCalendarSync(ctx, "initial", now, now.Add(time.Hour))
	if err != nil || initial == nil {
		t.Fatalf("initial claim: %v", err)
	}
	// Simulate an already synchronized legacy cursor before enabling copy mode.
	next := now.Add(time.Second)
	if err = s.ApplyEventSyncPage(ctx, *initial, storage.EventSyncBatch{NextCursor: "legacy-cursor", NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertCalendar(ctx, storage.Calendar{ID: "google:target", ConnectionID: "connection", ProviderCalendarID: "target", Name: "target", Timezone: "UTC", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "google:target", SyncMode: "cached_instances", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err = s.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "event", CalendarID: "provider-local", Provider: "google", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	provider := &fakeProvider{providerName: "google", pages: []calendar.EventSyncPage{{SourceTimeZone: "UTC", NextPageToken: "page-2", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: event.ID}, Event: event}}}, {SourceTimeZone: "UTC", Complete: true, NextCursor: "replica-cursor"}, {SourceTimeZone: "UTC", Complete: true, NextCursor: "next-cursor", DeletedEventIDs: []string{"event"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "event", Reason: calendar.EventSyncProviderCancellation}}}}}
	clock := next
	svc := NewService(s, fakeResolver{provider: provider, rawID: "source"}, nil)
	svc.Now = func() time.Time { return clock }
	state, err := s.ClaimDueCalendarSync(ctx, "source-worker", clock, clock.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != "google:source" {
		t.Fatalf("source claim: %v", err)
	}
	if err = svc.RunOne(ctx, *state); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 || provider.requests[0].Mode != calendar.EventSyncReplacement || provider.requests[0].Cursor != "" || provider.requests[1].PageToken != "page-2" {
		t.Fatal("bootstrap did not reuse one replacement feed")
	}
	result, err := s.GetSourceProjectionForCopy(ctx, "google:source", window.Start, window.End, clock.Add(time.Second), time.Hour)
	if err != nil || len(result.Events) != 1 || result.Snapshot.Cursor != "replica-cursor" {
		t.Fatalf("bootstrap snapshot: %v", err)
	}
	clock = clock.Add(5 * time.Minute)
	state, err = s.ClaimDueCalendarSync(ctx, "incremental-worker", clock, clock.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != "google:source" {
		t.Fatalf("incremental claim: %v", err)
	}
	if err = svc.RunOne(ctx, *state); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 3 || provider.requests[2].Mode != calendar.EventSyncIncremental || provider.requests[2].Cursor != "replica-cursor" {
		t.Fatal("incremental feed/cursor mismatch")
	}
	result, err = s.GetSourceProjectionForCopy(ctx, "google:source", window.Start, window.End, clock.Add(time.Second), time.Hour)
	if err != nil || len(result.Events) != 0 || len(result.Observations) != 1 || result.Observations[0].Unresolved || result.Observations[0].PreviousIdentity == "" {
		t.Fatalf("incremental cancellation: %v", err)
	}
	// A malformed provider page must not overwrite the last complete snapshot
	// or advance the source cursor past unprocessed data.
	provider.pages = append(provider.pages, calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "must-not-publish", Warnings: []calendar.EventSyncWarning{{Code: calendar.EventSyncProtocol, ObjectID: "malformed"}}})
	clock = clock.Add(5 * time.Minute)
	state, err = s.ClaimDueCalendarSync(ctx, "degraded-worker", clock, clock.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != "google:source" {
		t.Fatalf("degraded claim: %v", err)
	}
	if err = svc.RunOne(ctx, *state); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetSourceProjectionForCopy(ctx, "google:source", window.Start, window.End, clock.Add(time.Second), time.Hour); err == nil {
		t.Fatal("degraded projection accepted")
	}
	last, err := s.GetProjectionSnapshot(ctx, "google:source", 2)
	if err != nil || last.Cursor != "next-cursor" {
		t.Fatalf("degraded page changed published revision: %v", err)
	}
}
