package application

import (
	"context"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

const reconciliationWarning = "Calendar data will refresh shortly."

func (s *Service) reconcileCreatedEvent(ctx context.Context, event calendar.EventV2) []string {
	if s.eventReadModel == nil {
		return nil
	}
	now := time.Now().UTC()
	projectionFailed := s.eventReadModel.UpsertCachedEvent(ctx, eventForReadModel(event), now) != nil
	if s.ScheduleCalendarSync(ctx, event.CalendarID, now) != nil {
		projectionFailed = true
	}
	if projectionFailed {
		return []string{reconciliationWarning}
	}
	return nil
}

func (s *Service) reconcileOperation(ctx context.Context, result *calendar.OperationResult, ref calendar.EventRef, deleted bool) {
	if s.eventReadModel == nil || result == nil || result.Status == "preview" {
		return
	}
	now := time.Now().UTC()
	projectionFailed := false
	if deleted {
		if err := s.eventReadModel.DeleteCachedEvent(ctx, ref, now); err != nil {
			projectionFailed = true
		}
	} else {
		if result.Event != nil {
			if err := s.eventReadModel.UpsertCachedEvent(ctx, eventForReadModel(*result.Event), now); err != nil {
				projectionFailed = true
			}
		}
		for _, event := range result.RelatedEvents {
			if err := s.eventReadModel.UpsertCachedEvent(ctx, eventForReadModel(event), now); err != nil {
				projectionFailed = true
			}
		}
	}
	if err := s.ScheduleCalendarSync(ctx, ref.CalendarID, now); err != nil {
		projectionFailed = true
	}
	if projectionFailed {
		result.Warnings = append(result.Warnings, reconciliationWarning)
	}
}

// Event-sync adapters persist provider-local event keys. Keep write-through on
// that same identity so the next incremental sync updates one cache row rather
// than creating a prefixed duplicate. Browser serialization restores prefixes.
func eventForReadModel(event calendar.EventV2) calendar.EventV2 {
	if event.Provider == "" {
		return event
	}
	event.ID = strings.TrimPrefix(event.ID, event.Provider+":")
	event.RecurringEventID = strings.TrimPrefix(event.RecurringEventID, event.Provider+":")
	return event
}
