package application

import (
	"context"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type writerTicketKey struct{}
type writerTicket struct {
	calendarID string
	ticket     storage.AuthorityTicket
	err        error
}
type guardedWriter interface {
	CaptureWriterTicket(context.Context, string) (storage.AuthorityTicket, error)
	UpsertCachedEventWithTicket(context.Context, storage.AuthorityTicket, calendar.EventV2, time.Time) error
	DeleteCachedEventWithTicket(context.Context, storage.AuthorityTicket, calendar.EventRef, time.Time) error
}

func (s *Service) captureWriter(ctx context.Context, provider calendar.Provider, rawCalendarID string) context.Context {
	if writer, ok := s.eventReadModel.(guardedWriter); ok {
		calendarID := calendar.CanonicalCalendarID(provider, rawCalendarID)
		t, err := writer.CaptureWriterTicket(ctx, calendarID)
		if bound, ok := provider.(interface{ CredentialBinding() (string, [32]byte) }); ok && err == nil {
			if revision, _ := bound.CredentialBinding(); revision != t.AuthorizationRevision {
				err = storage.ErrAuthorityChanged
			}
		}
		return context.WithValue(ctx, writerTicketKey{}, writerTicket{calendarID: calendarID, ticket: t, err: err})
	}
	return ctx
}
func projectionCalendarID(ctx context.Context, calendarID string) string {
	if writer, ok := ctx.Value(writerTicketKey{}).(writerTicket); ok {
		return writer.calendarID
	}
	return calendarID
}

func (s *Service) guardedUpsert(ctx context.Context, event calendar.EventV2, now time.Time) error {
	if writer, ok := s.eventReadModel.(guardedWriter); ok {
		v, exists := ctx.Value(writerTicketKey{}).(writerTicket)
		if !exists {
			return storage.ErrAuthorityChanged
		}
		if v.err != nil {
			return v.err
		}
		event.CalendarID = v.ticket.CalendarID
		return writer.UpsertCachedEventWithTicket(ctx, v.ticket, event, now)
	}
	return s.eventReadModel.UpsertCachedEvent(ctx, event, now)
}
func (s *Service) guardedDelete(ctx context.Context, ref calendar.EventRef, now time.Time) error {
	if writer, ok := s.eventReadModel.(guardedWriter); ok {
		v, exists := ctx.Value(writerTicketKey{}).(writerTicket)
		if !exists {
			return storage.ErrAuthorityChanged
		}
		if v.err != nil {
			return v.err
		}
		ref.CalendarID = v.ticket.CalendarID
		return writer.DeleteCachedEventWithTicket(ctx, v.ticket, ref, now)
	}
	return s.eventReadModel.DeleteCachedEvent(ctx, ref, now)
}

const reconciliationWarning = "Calendar data will refresh shortly."

func (s *Service) reconcileCreatedEvent(ctx context.Context, event calendar.EventV2) []string {
	if s.eventReadModel == nil {
		return nil
	}
	now := time.Now().UTC()
	projectionFailed := s.guardedUpsert(ctx, eventForReadModel(event), now) != nil
	if s.ScheduleCalendarSync(ctx, projectionCalendarID(ctx, event.CalendarID), now) != nil {
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
	if result.Status != "completed" {
		if s.ScheduleCalendarSync(ctx, projectionCalendarID(ctx, ref.CalendarID), now) != nil {
			result.Warnings = append(result.Warnings, reconciliationWarning)
		}
		return
	}
	projectionFailed := false
	if deleted {
		if err := s.guardedDelete(ctx, ref, now); err != nil {
			projectionFailed = true
		}
	} else {
		if result.Event != nil {
			if err := s.guardedUpsert(ctx, eventForReadModel(*result.Event), now); err != nil {
				projectionFailed = true
			}
		}
		for _, event := range result.RelatedEvents {
			if err := s.guardedUpsert(ctx, eventForReadModel(event), now); err != nil {
				projectionFailed = true
			}
		}
	}
	if err := s.ScheduleCalendarSync(ctx, projectionCalendarID(ctx, ref.CalendarID), now); err != nil {
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
	// Partial or otherwise unconfirmed deletion must not hide the cached event.
	// Ask the provider sync to reconcile it instead; an active mutation fence
	// continues to block copy-safe publication until the outcome is known.
	if result.Status != "completed" {
		if s.ScheduleCalendarSync(ctx, projectionCalendarID(ctx, ref.CalendarID), now) != nil {
			result.Warnings = append(result.Warnings, reconciliationWarning)
		}
		return
	}
	projectionFailed := false
	if scope == calendar.ScopeSeries {
		if err := s.deleteCachedSeries(ctx, ref, now); err != nil {
			projectionFailed = true
		}
	} else if err := s.guardedDelete(ctx, ref, now); err != nil {
		projectionFailed = true
	}
	// Following-scope implementations can return the trimmed master and a
	// replacement series. Persist every confirmed result before scheduling the
	// provider reconciliation pass.
	if result.Event != nil {
		if err := s.guardedUpsert(ctx, eventForReadModel(*result.Event), now); err != nil {
			projectionFailed = true
		}
	}
	for _, event := range result.RelatedEvents {
		if err := s.guardedUpsert(ctx, eventForReadModel(event), now); err != nil {
			projectionFailed = true
		}
	}
	if err := s.ScheduleCalendarSync(ctx, projectionCalendarID(ctx, ref.CalendarID), now); err != nil {
		projectionFailed = true
	}
	if projectionFailed {
		result.Warnings = append(result.Warnings, reconciliationWarning)
	}
}

func (s *Service) deleteCachedSeries(ctx context.Context, ref calendar.EventRef, now time.Time) error {
	if !s.eventReadModelWindow.End.After(s.eventReadModelWindow.Start) {
		return s.guardedDelete(ctx, ref, now)
	}
	events, _, err := s.eventReadModel.ListCachedEvents(ctx, []string{projectionCalendarID(ctx, ref.CalendarID)}, s.eventReadModelWindow.Start, s.eventReadModelWindow.End)
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
		if err := s.guardedDelete(ctx, calendar.EventRef{CalendarID: ref.CalendarID, EventID: event.ID}, now); err != nil {
			return err
		}
		deleted = true
	}
	if deleted {
		return nil
	}
	return s.guardedDelete(ctx, ref, now)
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
