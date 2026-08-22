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

func (s *Service) reconcileDeleteOperation(ctx context.Context, result *calendar.OperationResult, ref calendar.EventRef, scope calendar.MutationScope) {
	if s.eventReadModel == nil || result == nil || result.Status == "preview" {
		return
	}
	now := time.Now().UTC()
	projectionFailed := false
	if scope == calendar.ScopeSeries {
		if err := s.deleteCachedSeries(ctx, ref, now); err != nil {
			projectionFailed = true
		}
	} else if err := s.eventReadModel.DeleteCachedEvent(ctx, ref, now); err != nil {
		projectionFailed = true
	}
	// Following-scope implementations can return the trimmed master and a
	// replacement series. Persist every confirmed result before scheduling the
	// provider reconciliation pass.
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
	if err := s.ScheduleCalendarSync(ctx, ref.CalendarID, now); err != nil {
		projectionFailed = true
	}
	if projectionFailed {
		result.Warnings = append(result.Warnings, reconciliationWarning)
	}
}

func (s *Service) deleteCachedSeries(ctx context.Context, ref calendar.EventRef, now time.Time) error {
	if !s.eventReadModelWindow.End.After(s.eventReadModelWindow.Start) {
		return s.eventReadModel.DeleteCachedEvent(ctx, ref, now)
	}
	events, _, err := s.eventReadModel.ListCachedEvents(ctx, []string{ref.CalendarID}, s.eventReadModelWindow.Start, s.eventReadModelWindow.End)
	if err != nil {
		return err
	}
	seriesID := ref.EventID
	for _, event := range events {
		if event.ID == ref.EventID && event.RecurringEventID != "" {
			seriesID = event.RecurringEventID
			break
		}
	}
	deleted := false
	for _, event := range events {
		if event.ID != ref.EventID && event.ID != seriesID && event.RecurringEventID != seriesID {
			continue
		}
		if err := s.eventReadModel.DeleteCachedEvent(ctx, calendar.EventRef{CalendarID: ref.CalendarID, EventID: event.ID}, now); err != nil {
			return err
		}
		deleted = true
	}
	if deleted {
		return nil
	}
	return s.eventReadModel.DeleteCachedEvent(ctx, ref, now)
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
