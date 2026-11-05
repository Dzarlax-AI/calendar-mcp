package calendarprojection

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

// Candidate is a validated standalone source instance from a single
// already-published cache revision. It does not imply a target write.
type Candidate struct {
	Identity string
	Event    calendar.EventV2
}

// Candidates selects overlapping active instances from a complete,
// immutable snapshot. Missing, malformed and duplicate identities fail closed.
// Cancellation and eviction require separate provenance and are not inferred
// from absence in this collection.
func Candidates(sourceCalendarID string, events []calendar.EventV2, start, end time.Time, sourceZone *time.Location) ([]Candidate, error) {
	if sourceCalendarID == "" || strings.ContainsRune(sourceCalendarID, '\x00') {
		return nil, errors.New("source calendar ID is required")
	}
	if sourceZone == nil {
		return nil, errors.New("source calendar time zone is required")
	}
	if !end.After(start) {
		return nil, errors.New("cache copy window is invalid")
	}
	seen := make(map[string]struct{}, len(events))
	result := make([]Candidate, 0, len(events))
	for _, event := range events {
		if event.Status == "cancelled" || event.InstanceKind == "cancelled" {
			return nil, errors.New("cancelled events require explicit provenance")
		}
		if event.InstanceKind == "seriesMaster" || len(event.Recurrence) != 0 {
			return nil, errors.New("cache copy requires expanded instances")
		}
		if err := calendar.ValidateEventTimeRangeV2(event.Start, event.End); err != nil {
			return nil, fmt.Errorf("invalid cached event range: %w", err)
		}
		if event.OriginalStart != nil && event.OriginalStart.IsAllDay() {
			date := event.OriginalStart.Date
			instant, err := time.ParseInLocation("2006-01-02", date, sourceZone)
			if err != nil || instant.Format("2006-01-02") != date {
				return nil, errors.New("invalid local original start")
			}
		}
		identity, err := Identity(sourceCalendarID, event)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[identity]; exists {
			return nil, errors.New("duplicate cached occurrence identity")
		}
		seen[identity] = struct{}{}
		from, _ := event.Start.Instant()
		until, _ := event.End.Instant()
		if event.Start.IsAllDay() {
			var err error
			from, err = time.ParseInLocation("2006-01-02", event.Start.Date, sourceZone)
			if err != nil || from.Format("2006-01-02") != event.Start.Date {
				return nil, errors.New("invalid local all-day start")
			}
			until, err = time.ParseInLocation("2006-01-02", event.End.Date, sourceZone)
			if err != nil || until.Format("2006-01-02") != event.End.Date || !until.After(from) {
				return nil, errors.New("invalid local all-day end")
			}
		}
		if !from.Before(end) || !until.After(start) {
			continue
		}
		result = append(result, Candidate{Identity: identity, Event: event})
	}
	return result, nil
}
