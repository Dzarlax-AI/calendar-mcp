package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type noncompletedReviewProvider struct {
	*fakeV2Provider
	result *calendar.OperationResult
}

func (p *noncompletedReviewProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updated++
	return p.result, nil
}
func (p *noncompletedReviewProvider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.deleted++
	return p.result, nil
}

func TestNoncompletedMutationsRemainPending(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		for _, result := range []*calendar.OperationResult{nil, {Status: "pending"}, {Status: "preview"}} {
			event := calendar.EventV2{ID: "event", Title: "changed", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}}
			if cancelled {
				event.Status = "cancelled"
			}
			source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
			target := &noncompletedReviewProvider{fakeV2Provider: &fakeV2Provider{name: "apple"}, result: result}
			id := newMappingID("rule", event.ID, "")
			mappings := &fakeMappings{values: map[string]storage.Mapping{id: {ID: id, RuleID: "rule", SourceEventID: event.ID, TargetEventID: "target", ContentHash: "old", ReconciliationState: "current", Version: 1}}}
			engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
			rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
			if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrPendingTargetMutation) {
				t.Fatalf("cancelled=%v result=%+v: %v", cancelled, result, err)
			}
			want := "pending_update"
			if cancelled {
				want = "pending_delete"
			}
			if got := mappings.values[id]; got.ReconciliationState != want || got.ContentHash != "old" {
				t.Fatalf("cancelled=%v result=%+v: mapping=%+v", cancelled, result, got)
			}
		}
	}
}
