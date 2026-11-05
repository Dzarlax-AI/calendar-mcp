package syncengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/google"
	"calendar-mcp/internal/microsoft"
	"calendar-mcp/internal/storage"
)

type standaloneOnlyProvider struct{ calendar.EventProviderV2 }

func (standaloneOnlyProvider) SupportsNoAttendeeMutations() bool { return true }
func (p standaloneOnlyProvider) ValidateRecurrenceWrite(lines []string, start calendar.EventTime) error {
	return p.EventProviderV2.(calendar.RecurrenceWriteValidator).ValidateRecurrenceWrite(lines, start)
}
func (p standaloneOnlyProvider) FindEventBySyncMarkerV2(ctx context.Context, calendarID, ruleID, sourceEventID string) (*calendar.EventV2, error) {
	return p.EventProviderV2.(calendar.SyncMarkerLookupProvider).FindEventBySyncMarkerV2(ctx, calendarID, ruleID, sourceEventID)
}

func TestSafeRecurrencePreflightRejectsMixedRunBeforeTargetWrites(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	standalone := calendar.EventV2{ID: "standalone", Start: calendar.EventTime{Date: "2026-08-20"}, End: calendar.EventTime{Date: "2026-08-21"}}
	series := calendar.EventV2{ID: "series", SourceAttendeesComplete: true, InstanceKind: "seriesMaster", Start: standalone.Start, End: standalone.End, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{standalone, series}}
	real := &fakeV2Provider{name: "microsoft"}
	target := standaloneOnlyProvider{EventProviderV2: real}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	for _, dry := range []bool{true, false} {
		if _, err := engine.Run(t.Context(), rule, dry); !errors.Is(err, ErrUnsafeRecurringSync) {
			t.Fatalf("dry=%v: %v", dry, err)
		}
		if real.created+real.updated+real.deleted != 0 {
			t.Fatal("mixed run reached target mutation")
		}
	}
}

func TestSafeRecurrencePreflightAllowsUnchangedSeriesAlongsideStandalone(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	standalone := calendar.EventV2{ID: "standalone", Start: calendar.EventTime{Date: "2026-08-20"}, End: calendar.EventTime{Date: "2026-08-21"}}
	series := calendar.EventV2{ID: "series", SourceAttendeesComplete: true, InstanceKind: "seriesMaster", Start: standalone.Start, End: standalone.End, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{standalone, series}}
	real := &fakeV2Provider{name: "microsoft"}
	target := standaloneOnlyProvider{EventProviderV2: real}
	mapping := storage.Mapping{ID: "series-map", RuleID: "rule", ObjectKind: "series", SourceEventID: "series", TargetEventID: "target-series", ContentHash: hashEvent(series), ReconciliationState: "current"}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{mapping.ID: mapping}})
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 1 || real.created != 1 {
		t.Fatalf("unchanged series blocked standalone: %v", err)
	}
}

func TestRealProvidersDoNotAdvertiseSafeRecurringMutations(t *testing.T) {
	for _, provider := range []any{&google.Provider{}, &microsoft.Provider{}} {
		if _, ok := provider.(calendar.SafeNoAttendeeRecurringMutationProvider); ok {
			t.Fatal("real provider unexpectedly advertises safe recurring mutations")
		}
	}
}

func TestSafeRecurrencePreflightBlocksSeriesDeletionAndExceptionRestore(t *testing.T) {
	cancelled := calendar.EventV2{ID: "series", Status: "cancelled"}
	series := storage.Mapping{ID: "series-map", RuleID: "rule", ObjectKind: "series", SourceEventID: "series", TargetEventID: "target-series"}
	if err := preflightSafeRecurrence([]calendar.EventV2{cancelled}, map[string]storage.Mapping{mappingKey("series", ""): series}, []storage.Mapping{series}); !errors.Is(err, ErrUnsafeRecurringSync) {
		t.Fatalf("series deletion accepted: %v", err)
	}
	start := calendar.EventTime{Date: "2026-11-05"}
	occurrence := calendar.EventV2{ID: "ordinary-instance", InstanceKind: "occurrence", RecurringEventID: "series", OriginalStart: &start}
	exception := storage.Mapping{ID: "exception-map", RuleID: "rule", ObjectKind: "exception", SourceEventID: "old-exception", SourceSeriesID: "series", OriginalStart: eventOriginalStart(occurrence)}
	if err := preflightSafeRecurrence([]calendar.EventV2{occurrence}, map[string]storage.Mapping{}, []storage.Mapping{exception}); !errors.Is(err, ErrUnsafeRecurringSync) {
		t.Fatalf("exception restoration accepted: %v", err)
	}
}

func TestSafeRecurrencePreflightAllowsPendingIncompleteRestoreWithoutWrite(t *testing.T) {
	start := calendar.EventTime{Date: "2026-11-05"}
	end := calendar.EventTime{Date: "2026-11-06"}
	occurrence := calendar.EventV2{ID: "occurrence", InstanceKind: "occurrence", RecurringEventID: "series", OriginalStart: &start, Start: start, End: end}
	standalone := calendar.EventV2{ID: "standalone", Title: "changed", SourceAttendeesComplete: true, Start: start, End: end}
	exception := storage.Mapping{ID: "exception-map", RuleID: "rule", ObjectKind: "exception", SourceEventID: "old-exception", SourceSeriesID: "series", OriginalStart: eventOriginalStart(occurrence), TargetEventID: "target-instance", ContentHash: incompleteRestoreHash(occurrence)}
	single := storage.Mapping{ID: "single-map", RuleID: "rule", ObjectKind: "event", SourceEventID: "standalone", TargetEventID: "target-single", ContentHash: "old"}
	source := &fakeV2Provider{name: "google", sourceCompletenessExplicit: true, events: []calendar.EventV2{occurrence, standalone}}
	real := &fakeV2Provider{name: "microsoft"}
	target := standaloneOnlyProvider{EventProviderV2: real}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{exception.ID: exception, single.ID: single}})
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || result.Updated != 1 || real.updated != 1 || real.lastUpdate.Ref.EventID != "target-single" {
		t.Fatalf("pending restoration blocked standalone update or caused recurring write: result=%+v err=%v", result, err)
	}
	occurrence.SourceAttendeesComplete = true
	if err := preflightSafeRecurrence([]calendar.EventV2{occurrence}, map[string]storage.Mapping{}, []storage.Mapping{exception}); !errors.Is(err, ErrUnsafeRecurringSync) {
		t.Fatalf("completed restoration must remain blocked: %v", err)
	}
}
