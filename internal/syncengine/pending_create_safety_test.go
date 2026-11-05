package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type timedOutCreateProvider struct{ *fakeV2Provider }

func (p *timedOutCreateProvider) CreateEventV2(ctx context.Context, req calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	_, _ = p.fakeV2Provider.CreateEventV2(ctx, req)
	return nil, context.DeadlineExceeded
}

func TestPendingCreateNeverRetriesOnMissingMarker(t *testing.T) {
	for _, cancelSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "cancelled"}[cancelSource], func(t *testing.T) {
			source := &fakeV2Provider{name: "google", events: []calendar.EventV2{{ID: "event", Title: "meeting", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}}}}
			target := &timedOutCreateProvider{&fakeV2Provider{name: "apple"}}
			mappings := &fakeMappings{values: map[string]storage.Mapping{}}
			engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
			rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
			if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first run: %v", err)
			}
			if len(mappings.values) != 1 || target.created != 1 {
				t.Fatalf("missing intent or create: %d %d", len(mappings.values), target.created)
			}
			if cancelSource {
				source.events[0].Status = "cancelled"
			}
			if _, err := engine.Run(t.Context(), rule, true); !errors.Is(err, ErrPendingCreateUnresolved) {
				t.Fatalf("dry run: %v", err)
			}
			if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrPendingCreateUnresolved) {
				t.Fatalf("retry: %v", err)
			}
			if target.created != 1 || target.deleted != 0 || len(mappings.values) != 1 {
				t.Fatal("unresolved create was retried or forgotten")
			}
		})
	}
}

func TestPendingCreateAfterMappingCommitFailureIsNotRetried(t *testing.T) {
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{{ID: "event", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}}}}
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}, failCurrent: true}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); err == nil {
		t.Fatal("expected mapping failure")
	}
	mappings.failCurrent = false
	if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrPendingCreateUnresolved) {
		t.Fatalf("retry: %v", err)
	}
	if target.created != 1 || len(mappings.values) != 1 {
		t.Fatal("ambiguous create was retried")
	}
}

type failedRecoveryUpdateProvider struct{ *fakeV2Provider }

func (p *failedRecoveryUpdateProvider) UpdateEventV2(ctx context.Context, req calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	_, _ = p.fakeV2Provider.UpdateEventV2(ctx, req)
	return nil, errors.New("synthetic update failure")
}

func TestRecoveredStandaloneCopyUpdatedBeforeMapping(t *testing.T) {
	for _, failUpdate := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "update failure"}[failUpdate], func(t *testing.T) {
			sourceEvent := calendar.EventV2{ID: "event", Title: "new title", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}, SourceAttendeesComplete: true}
			old := sourceEvent
			old.ID = "target-event"
			old.Title = "old title"
			source := &fakeV2Provider{name: "google", events: []calendar.EventV2{sourceEvent}}
			target := &fakeV2Provider{name: "apple", recovered: &old}
			var provider calendar.Provider = target
			if failUpdate {
				provider = &failedRecoveryUpdateProvider{target}
			}
			id := newMappingID("rule", sourceEvent.ID, "")
			mappings := &fakeMappings{values: map[string]storage.Mapping{id: {ID: id, RuleID: "rule", ObjectKind: "event", SourceEventID: sourceEvent.ID, TargetEventID: "pending:" + id, ReconciliationState: "pending_create"}}}
			engine := testNew(calendar.NewRegistry([]calendar.Provider{source, provider}), mappings)
			rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
			_, err := engine.Run(t.Context(), rule, false)
			if (err != nil) != failUpdate {
				t.Fatalf("run error: %v", err)
			}
			if target.created != 0 || target.updated != 1 || target.lastUpdate.Patch.Title.Value != "new title" || !target.lastUpdate.RequireNoAttendees || target.lastUpdate.Notifications != calendar.NotificationsNone || target.lastUpdate.Patch.Attendees.Present {
				t.Fatalf("unsafe recovery update: %+v", target.lastUpdate)
			}
			got := mappings.values[id]
			if failUpdate {
				if got.ReconciliationState != "pending_update" || got.ContentHash == hashEvent(sourceEvent) {
					t.Fatalf("failed update committed mapping: %+v", got)
				}
			} else if got.ReconciliationState != "current" || got.ContentHash != hashEvent(sourceEvent) {
				t.Fatalf("updated mapping: %+v", got)
			}
		})
	}
}
