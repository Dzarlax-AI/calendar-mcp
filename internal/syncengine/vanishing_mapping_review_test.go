package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type vanishingReviewMappings struct{ *fakeMappings }

func (m *vanishingReviewMappings) UpdateMappingCAS(_ context.Context, value storage.Mapping, _ int64) error {
	delete(m.values, value.ID)
	return storage.ErrMappingConflict
}

func TestDisappearingMappingStopsBeforeDispatch(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		event := calendar.EventV2{ID: "event", Title: "changed", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}}
		if cancelled {
			event.Status = "cancelled"
		}
		source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
		target := &fakeV2Provider{name: "apple"}
		id := newMappingID("rule", event.ID, "")
		mappings := &vanishingReviewMappings{&fakeMappings{values: map[string]storage.Mapping{id: {ID: id, RuleID: "rule", SourceEventID: event.ID, TargetEventID: "target", ContentHash: "old", ReconciliationState: "current", Version: 1}}}}
		engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
		rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
		if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, storage.ErrMappingConflict) {
			t.Fatalf("cancelled=%v: %v", cancelled, err)
		}
		if target.updated != 0 || target.deleted != 0 {
			t.Fatal("provider called after mapping disappeared")
		}
	}
}
