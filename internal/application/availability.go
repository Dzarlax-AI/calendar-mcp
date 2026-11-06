package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

// ListAvailabilityEvents performs a bounded, single-calendar expanded read.
// Date-only events are included whenever any possible calendar timezone could
// overlap the requested window; date values remain unchanged. Consumers must
// treat those all-day events conservatively rather than as exact UTC intervals.
// Every page is drained here, so no provider page token escapes this endpoint.
func (s *Service) ListAvailabilityEvents(ctx context.Context, calendarID string, start, end time.Time) (calendar.Page[calendar.EventV2], error) {
	if err := validateRange(start, end); err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	route, raw, ok := strings.Cut(calendarID, ":")
	provider, account, hasAccount := strings.Cut(route, "@")
	if !hasAccount {
		account, raw, hasAccount = strings.Cut(raw, ":")
	}
	if !ok || !hasAccount || account == "" || strings.TrimSpace(calendarID) != calendarID || raw == "" || raw == "primary" || (provider != "google" && provider != "microsoft" && provider != "apple") {
		return calendar.Page[calendar.EventV2]{}, invalidArgument("exact account calendar_id is required")
	}
	// ±24 h is a conservative bound on civil UTC offsets, including date changes.
	req := calendar.ListEventsRequestV2{CalendarID: calendarID, Start: start.Add(-24 * time.Hour), End: end.Add(24 * time.Hour), View: calendar.RecurrenceExpanded, MaxResults: 1000, AvailabilityStrict: true}
	result := calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{}, Complete: true}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		page, err := s.ListEvents(ctx, req)
		if err != nil {
			return calendar.Page[calendar.EventV2]{}, err
		}
		if !page.Complete || len(page.Sources) != 1 || !page.Sources[0].Complete || page.Sources[0].CalendarID != calendarID || page.Sources[0].Provider != provider || page.Sources[0].Error != nil {
			return calendar.Page[calendar.EventV2]{}, fmt.Errorf("availability source is incomplete")
		}
		for _, event := range page.Items {
			switch event.InstanceKind {
			case "", "occurrence", "exception", "cancelled", "orphanOccurrence":
			default:
				return calendar.Page[calendar.EventV2]{}, fmt.Errorf("availability requires expanded instances")
			}
			if strings.EqualFold(event.Status, "cancelled") || event.InstanceKind == "cancelled" {
				continue
			}
			if event.ID == "" || event.CalendarID != calendarID || event.Provider != provider || len(event.Recurrence) > 0 {
				return calendar.Page[calendar.EventV2]{}, fmt.Errorf("invalid availability event")
			}
			if err = calendar.ValidateEventTimeRangeV2(event.Start, event.End); err != nil {
				return calendar.Page[calendar.EventV2]{}, fmt.Errorf("invalid availability time range")
			}
			from, _ := event.Start.Instant()
			until, _ := event.End.Instant()
			if event.Start.IsAllDay() {
				from = from.Add(-24 * time.Hour)
				until = until.Add(24 * time.Hour)
			}
			if until.After(start) && from.Before(end) {
				result.Items = append(result.Items, event)
			}
			if len(result.Items) > 100000 {
				return calendar.Page[calendar.EventV2]{}, fmt.Errorf("availability event limit exceeded")
			}
		}
		if page.NextPageToken == "" {
			result.Sources = page.Sources
			return result, nil
		}
		if seen[page.NextPageToken] {
			return calendar.Page[calendar.EventV2]{}, fmt.Errorf("availability pagination repeated token")
		}
		seen[page.NextPageToken] = true
		req.PageToken = page.NextPageToken
	}
	return calendar.Page[calendar.EventV2]{}, fmt.Errorf("availability pagination limit exceeded")
}
