package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type reviewDispatchFence struct {
	calls int
	err   error
}

func (f *reviewDispatchFence) CheckProviderDispatch(context.Context) error { f.calls++; return f.err }

func TestDispatchFenceProtectsEachProviderMutation(t *testing.T) {
	source := &fakeV2Provider{name: "google"}
	target := &fakeV2Provider{name: "apple"}
	guard := selectionGuardedTarget{EventProviderV2: target, registry: calendar.NewRegistry([]calendar.Provider{source, target}), sourceID: "google:source", targetID: "apple:target"}
	calls := []func() error{
		func() error { _, err := guard.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{}); return err },
		func() error { _, err := guard.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{}); return err },
		func() error { _, err := guard.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{}); return err },
	}
	for _, call := range calls {
		if err := call(); !errors.Is(err, ErrDispatchFenceUnavailable) {
			t.Fatalf("nil fence allowed mutation: %v", err)
		}
	}
	denied := &reviewDispatchFence{err: storage.ErrJobLeaseLost}
	guard.dispatchFence = denied
	for _, call := range calls {
		if err := call(); !errors.Is(err, storage.ErrJobLeaseLost) {
			t.Fatalf("lost lease allowed mutation: %v", err)
		}
	}
	if denied.calls != 3 || target.created != 0 || target.updated != 0 || target.deleted != 0 {
		t.Fatalf("denied calls=%d creates=%d updates=%d deletes=%d", denied.calls, target.created, target.updated, target.deleted)
	}
	allowed := &reviewDispatchFence{}
	guard.dispatchFence = allowed
	for _, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("valid fence rejected: %v", err)
		}
	}
	if allowed.calls != 3 || target.created != 1 || target.updated != 1 || target.deleted != 1 {
		t.Fatalf("allowed calls=%d creates=%d updates=%d deletes=%d", allowed.calls, target.created, target.updated, target.deleted)
	}
}

func TestDryRunDoesNotRequireDispatchFence(t *testing.T) {
	start := calendar.EventTime{Date: "2026-10-01"}
	event := calendar.EventV2{ID: "event", Title: "Meeting", Start: start, End: calendar.EventTime{Date: "2026-10-02"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, true)
	if err != nil || result.Created != 1 {
		t.Fatalf("dry run: %+v %v", result, err)
	}
	if len(mappings.values) != 0 || target.created != 0 || target.updated != 0 || target.deleted != 0 {
		t.Fatal("dry run changed target or mapping")
	}
}

func TestExpiredFencePreservesMappedEvents(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		event := calendar.EventV2{ID: "event", Title: "changed", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}}
		if cancelled {
			event.Status = "cancelled"
		}
		source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
		target := &fakeV2Provider{name: "apple"}
		id := newMappingID("rule", event.ID, "")
		original := storage.Mapping{ID: id, RuleID: "rule", SourceEventID: event.ID, ObjectKind: "event", TargetEventID: "existing", ContentHash: "old", ReconciliationState: "current", Version: 1, Incarnation: "synthetic-incarnation"}
		mappings := &fakeMappings{values: map[string]storage.Mapping{id: original}}
		engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings).WithDispatchFence(&reviewDispatchFence{err: storage.ErrJobLeaseLost})
		rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
		if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, storage.ErrJobLeaseLost) {
			t.Fatalf("cancelled=%v: %v", cancelled, err)
		}
		if got := mappings.values[id]; got != original {
			t.Fatalf("cancelled=%v: mapping changed: %+v", cancelled, got)
		}
		if target.updated != 0 || target.deleted != 0 {
			t.Fatal("expired attempt called provider")
		}
	}
}
