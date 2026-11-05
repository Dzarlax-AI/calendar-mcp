package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type cancelledCreateReviewProvider struct{ *fakeV2Provider }

func (p *cancelledCreateReviewProvider) CreateEventV2(_ context.Context, _ calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.created++
	return &calendar.EventV2{ID: "synthetic-target", Status: "cancelled"}, nil
}

func TestCancelledCreateResultStaysPending(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		event := calendar.EventV2{ID: "event", Title: "Meeting", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}}
		source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
		target := &cancelledCreateReviewProvider{&fakeV2Provider{name: "apple"}}
		if recovery {
			target.recovered = &calendar.EventV2{ID: "synthetic-target", Status: "cancelled"}
		}
		mappings := &fakeMappings{values: map[string]storage.Mapping{}}
		engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
		rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
		if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrPendingCreateUnresolved) {
			t.Fatalf("recovery=%v: %v", recovery, err)
		}
		if len(mappings.values) != 1 {
			t.Fatalf("recovery=%v: missing pending mapping", recovery)
		}
		for _, m := range mappings.values {
			if m.ReconciliationState != "pending_create" {
				t.Fatalf("recovery=%v: mapping=%+v", recovery, m)
			}
		}
		if target.updated != 0 || target.deleted != 0 {
			t.Fatal("cancelled response caused further mutations")
		}
	}
}
