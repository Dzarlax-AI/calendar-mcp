package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type unknownMutationProvider struct{ *fakeV2Provider }

func (p *unknownMutationProvider) UpdateEventV2(ctx context.Context, req calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updated++
	return nil, errors.New("synthetic unknown outcome")
}
func (p *unknownMutationProvider) DeleteEventV2(ctx context.Context, req calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.deleted++
	return nil, errors.New("synthetic unknown outcome")
}

type refusedFence struct{}

func (refusedFence) CheckProviderDispatch(context.Context) error { return ErrDispatchFenceUnavailable }

func TestUnknownTargetMutationIsParked(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "delete"}[cancelled], func(t *testing.T) {
			event := calendar.EventV2{ID: "event", Title: "new", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
			if cancelled {
				event.Status = "cancelled"
			}
			source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
			target := &unknownMutationProvider{&fakeV2Provider{name: "microsoft"}}
			id := newMappingID("rule", event.ID, "")
			mappings := &fakeMappings{values: map[string]storage.Mapping{id: {ID: id, RuleID: "rule", ObjectKind: "event", SourceEventID: event.ID, TargetEventID: "target", ContentHash: "old", ReconciliationState: "current"}}}
			engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
			rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
			if _, err := engine.Run(t.Context(), rule, false); err == nil {
				t.Fatal("expected unknown provider outcome")
			}
			expected := "pending_update"
			if cancelled {
				expected = "pending_delete"
			}
			if got := mappings.values[id].ReconciliationState; got != expected {
				t.Fatalf("intent=%s", got)
			}
			if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrPendingTargetMutation) {
				t.Fatalf("retry not blocked: %v", err)
			}
			if _, err := engine.Run(t.Context(), rule, true); !errors.Is(err, ErrPendingTargetMutation) {
				t.Fatalf("dry-run not blocked: %v", err)
			}
			if target.updated+target.deleted != 1 {
				t.Fatalf("repeated provider call: %d", target.updated+target.deleted)
			}
		})
	}
}
func TestExpiredFenceDoesNotCreatePendingIntent(t *testing.T) {
	event := calendar.EventV2{ID: "event", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
	target := &fakeV2Provider{name: "microsoft"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings).WithDispatchFence(refusedFence{})
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrDispatchFenceUnavailable) {
		t.Fatalf("fence: %v", err)
	}
	if len(mappings.values) != 0 || target.created != 0 {
		t.Fatal("expired job created intent or dispatched")
	}
}

func TestExistingMappingRejectsDifferentTargetBeforeDispatch(t *testing.T) {
	mappings := &fakeMappings{values: map[string]storage.Mapping{"mapping": {ID: "mapping", RuleID: "rule", SourceEventID: "event", TargetEventID: "old-target", ReconciliationState: "current", Version: 1}}}
	engine := testNew(calendar.NewRegistry(nil), mappings)
	original := mappings.values["mapping"]
	if _, err := engine.beginMappingMutation(t.Context(), original, "pending_delete", "other-target"); !errors.Is(err, ErrPendingTargetMutation) {
		t.Fatalf("target mismatch: %v", err)
	}
	if got := mappings.values["mapping"]; got.ReconciliationState != "current" || got.TargetEventID != "old-target" || got.Version != 1 {
		t.Fatal("target mismatch changed mapping")
	}
}
