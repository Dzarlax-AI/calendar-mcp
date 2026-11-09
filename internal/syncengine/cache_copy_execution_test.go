package syncengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
	"calendar-mcp/internal/storage"
)

func TestSyntheticCachedBatchUsesStandaloneIdentityAndExplicitCancellation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	end := calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	original := start
	event := calendar.EventV2{CalendarID: "google:source", ID: "source-instance", RecurringEventID: "series", OriginalStart: &original, Start: start, End: end, Title: "Meeting", SourceAttendeesComplete: true}
	identity, err := calendarprojection.Identity(event.CalendarID, event)
	if err != nil {
		t.Fatal(err)
	}
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	// There is deliberately no source provider in the registry.
	engine := testNew(calendar.NewRegistry([]calendar.Provider{target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "apple:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	batch := cacheExecutionBatch{Events: []calendar.EventV2{event}, Timezone: "UTC", WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(24 * time.Hour), HistoryComplete: true}
	result, err := engine.runCachedBatch(t.Context(), rule, batch, false)
	if err != nil || result.Created != 1 || target.created != 1 {
		t.Fatalf("create: %+v %v", result, err)
	}
	if len(target.lastCreate.Event.Attendees) != 0 || target.lastCreate.Notifications != calendar.NotificationsNone || len(target.lastCreate.Event.Recurrence) != 0 || !target.lastCreate.RequireNoAttendees {
		t.Fatalf("unsafe create: %+v", target.lastCreate)
	}
	// A moved occurrence can receive a different provider event ID. Its source
	// series and original start still identify the same target mapping.
	event.ID = "changed-source-id"
	event.Title = "Moved"
	event.Start = calendar.EventTime{DateTime: now.Add(3 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	event.End = calendar.EventTime{DateTime: now.Add(4 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	batch.Events = []calendar.EventV2{event}
	result, err = engine.runCachedBatch(t.Context(), rule, batch, false)
	if err != nil || result.Updated != 1 || target.created != 1 || target.updated != 1 {
		t.Fatalf("update: %+v %v", result, err)
	}
	batch.Events = nil
	batch.Removals = []calendarprojection.RemovalObservation{{Removal: calendar.EventSyncRemoval{ObjectID: "source-instance", Reason: calendar.EventSyncWindowEviction}, PreviousIdentity: identity}}
	if _, err = engine.runCachedBatch(t.Context(), rule, batch, false); err != nil || target.deleted != 0 {
		t.Fatalf("eviction deleted: %v", err)
	}
	batch.Removals = []calendarprojection.RemovalObservation{{Removal: calendar.EventSyncRemoval{ObjectID: "source-instance", Reason: calendar.EventSyncProviderCancellation}, PreviousIdentity: identity, Unresolved: true}}
	if result, err = engine.runCachedBatch(t.Context(), rule, batch, false); err != nil || result.Warnings == 0 || target.deleted != 0 {
		t.Fatalf("unresolved cancellation: %v", err)
	}
	batch.Removals[0].Unresolved = false
	result, err = engine.runCachedBatch(t.Context(), rule, batch, false)
	if err != nil || result.Deleted != 1 || target.deleted != 1 || len(mappings.values) != 0 {
		t.Fatalf("confirmed cancellation: %+v %v", result, err)
	}
	// A later active occurrence wins over the retained historical tombstone.
	batch.Events = []calendar.EventV2{event}
	result, err = engine.runCachedBatch(t.Context(), rule, batch, false)
	if err != nil || result.Created != 1 || target.created != 2 || target.deleted != 1 {
		t.Fatalf("restored event: %+v %v", result, err)
	}
	// The restored instance still exists in the wider source snapshot even
	// after moving outside this rule's window. Its old tombstone must not
	// delete the existing copy.
	outside := event
	outside.Start = calendar.EventTime{DateTime: now.Add(48 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	outside.End = calendar.EventTime{DateTime: now.Add(49 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	batch.Events = []calendar.EventV2{outside}
	result, err = engine.runCachedBatch(t.Context(), rule, batch, false)
	if err != nil || result.Deleted != 0 || target.deleted != 1 || len(mappings.values) != 1 {
		t.Fatalf("restored event outside rule window: %+v %v", result, err)
	}
	batch.Events = []calendar.EventV2{event}
	// A prior uncertain delete still blocks restoration before any write.
	for id, m := range mappings.values {
		m.ReconciliationState = "pending_delete"
		mappings.values[id] = m
	}
	_, err = engine.runCachedBatch(t.Context(), rule, batch, false)
	if !errors.Is(err, ErrPendingTargetMutation) || target.created != 2 || target.deleted != 1 {
		t.Fatalf("pending delete restoration: %v", err)
	}
}

type denyCacheReviewFence struct{}

func (denyCacheReviewFence) CheckProviderDispatch(context.Context) error {
	return storage.ErrJobLeaseLost
}

type syntheticCacheExecutionReader struct {
	value     storage.SourceProjectionForCopy
	checks    int
	failAfter int
}

func (r *syntheticCacheExecutionReader) GetSourceProjectionForCopy(context.Context, string, time.Time, time.Time, time.Time, time.Duration) (storage.SourceProjectionForCopy, error) {
	return r.value, nil
}
func (r *syntheticCacheExecutionReader) CheckSourceProjectionCurrent(_ context.Context, _ string, revision int64, _ time.Time, _ time.Time, _ time.Time, _ time.Duration) error {
	r.checks++
	if revision != r.value.Snapshot.Revision || r.failAfter > 0 && r.checks >= r.failAfter {
		return storage.ErrProjectionSnapshotUnavailable
	}
	return nil
}
func (r *syntheticCacheExecutionReader) CheckSyncTargetEligibility(context.Context, string) error {
	return nil
}

func TestCacheExecutionRechecksLocalSourceBeforeDispatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	event := calendar.EventV2{CalendarID: "google:source", ID: "event", Title: "Meeting", Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "apple:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	reader := &syntheticCacheExecutionReader{value: storage.SourceProjectionForCopy{Snapshot: storage.ProjectionSnapshot{CalendarID: event.CalendarID, Revision: 1, Timezone: "UTC", Verified: true}, Events: []calendar.EventV2{event}}, failAfter: 2}
	if _, err := engine.RunCacheCopy(t.Context(), reader, rule, now, time.Hour); !errors.Is(err, storage.ErrProjectionSnapshotUnavailable) {
		t.Fatalf("changed revision: %v", err)
	}
	if target.created != 0 {
		t.Fatal("stale source dispatched")
	}
	if len(mappings.values) != 1 {
		t.Fatal("unknown dispatch boundary did not retain intent")
	}
}

func TestSyntheticCachedBatchFailsClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	event := calendar.EventV2{CalendarID: "google:source", ID: "one", Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{target}), mappings).WithDispatchFence(denyCacheReviewFence{})
	rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "apple:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	batch := cacheExecutionBatch{Events: []calendar.EventV2{event}, Timezone: "UTC", WindowStart: now, WindowEnd: now.Add(24 * time.Hour), HistoryComplete: true}
	if _, err := engine.runCachedBatch(t.Context(), rule, batch, false); !errors.Is(err, storage.ErrJobLeaseLost) || target.created != 0 || len(mappings.values) != 0 {
		t.Fatalf("fence: %v", err)
	}
	batch.HistoryComplete = false
	if _, err := engine.runCachedBatch(t.Context(), rule, batch, false); !errors.Is(err, storage.ErrProjectionSnapshotUnavailable) {
		t.Fatalf("incomplete history: %v", err)
	}
}

func TestUnresolvedCancellationDoesNotBlockIndependentEvent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	event := calendar.EventV2{CalendarID: "google:source", ID: "unrelated", Title: "independent", Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "apple:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	batch := cacheExecutionBatch{Events: []calendar.EventV2{event}, Removals: []calendarprojection.RemovalObservation{{Removal: calendar.EventSyncRemoval{ObjectID: "unknown-historical", Reason: calendar.EventSyncProviderCancellation}, Unresolved: true}}, Timezone: "UTC", WindowStart: now, WindowEnd: now.Add(24 * time.Hour), HistoryComplete: true}
	result, err := engine.runCachedBatch(t.Context(), rule, batch, false)
	if err != nil || result.Created != 1 || result.Warnings == 0 || target.created != 1 || target.deleted != 0 {
		t.Fatalf("independent event: %+v %v", result, err)
	}
}

func TestUnresolvedCancellationWarningsCountDistinctObjectsWithoutDeleting(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	observation := func(id string) calendarprojection.RemovalObservation {
		return calendarprojection.RemovalObservation{Removal: calendar.EventSyncRemoval{ObjectID: id, Reason: calendar.EventSyncProviderCancellation}, Unresolved: true}
	}
	batch := cacheExecutionBatch{Removals: []calendarprojection.RemovalObservation{observation("one"), observation("one"), observation("two")}, Timezone: "UTC", WindowStart: now, WindowEnd: now.Add(24 * time.Hour), HistoryComplete: true}
	result, err := engine.runCachedBatch(t.Context(), rule, batch, false)
	if err != nil || result.UnresolvedCancellations != 2 || result.Warnings != 2 || target.deleted != 0 {
		t.Fatalf("result=%+v deleted=%d err=%v", result, target.deleted, err)
	}
}
