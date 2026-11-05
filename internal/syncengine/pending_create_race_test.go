package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

// staleMappingView simulates a worker that read before another worker inserted
// the identity. The insert still sees the current mapping.
type staleMappingView struct{ *fakeMappings }

func (m *staleMappingView) ListMappings(context.Context, string) ([]storage.Mapping, error) {
	return nil, nil
}

func TestEnginePendingInsertConflictStopsBeforeProvider(t *testing.T) {
	start := calendar.EventTime{Date: "2026-10-01"}
	event := calendar.EventV2{ID: "instance", Title: "Meeting", InstanceKind: calendar.OrphanOccurrence, RecurringEventID: "master", OriginalStart: &start, Start: start, End: calendar.EventTime{Date: "2026-10-02"}}
	id := newMappingID("rule", event.ID, eventOriginalStart(event))
	existing := storage.Mapping{ID: id, RuleID: "rule", SourceEventID: event.ID, OriginalStart: eventOriginalStart(event), TargetEventID: "real-existing", ReconciliationState: "current"}
	mappings := &staleMappingView{&fakeMappings{values: map[string]storage.Mapping{id: existing}}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
	target := &fakeV2Provider{name: "apple"}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrPendingCreateUnresolved) {
		t.Fatalf("stale mapping view: %v", err)
	}
	if target.lookupCount != 0 || target.created != 0 || target.updated != 0 || target.deleted != 0 {
		t.Fatalf("provider touched after conflict: lookup=%d create=%d update=%d delete=%d", target.lookupCount, target.created, target.updated, target.deleted)
	}
	if got := mappings.values[id]; got.TargetEventID != existing.TargetEventID || got.ReconciliationState != "current" {
		t.Fatalf("existing mapping changed: %+v", got)
	}
}
