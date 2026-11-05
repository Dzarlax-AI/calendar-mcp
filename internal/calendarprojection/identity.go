// Package calendarprojection contains pure validation and materialization
// primitives. It does not establish provider completeness or authorize writes.
package calendarprojection

import (
	"calendar-mcp/internal/calendar"
	"errors"
	"fmt"
	"strings"
	"time"
)

func mappingKey(id, original string) string { return id + "\x00" + original }

// Identity identifies one source object without relying on a shared
// recurrence UID. The original start, not the possibly moved start, is used.
func Identity(sourceCalendarID string, event calendar.EventV2) (string, error) {
	if sourceCalendarID == "" || strings.ContainsRune(sourceCalendarID, '\x00') {
		return "", errors.New("source calendar ID is required")
	}
	if event.CalendarID != sourceCalendarID {
		return "", errors.New("event does not belong to source calendar")
	}
	if event.ID == "" || strings.ContainsRune(event.ID, '\x00') || strings.ContainsRune(event.RecurringEventID, '\x00') {
		return "", errors.New("source event ID is required")
	}
	if event.RecurringEventID == "" {
		if event.OriginalStart != nil {
			return "", errors.New("occurrence has no source series identity")
		}
		return mappingKey(sourceCalendarID, mappingKey(event.ID, "")), nil
	}
	if event.OriginalStart == nil {
		return "", errors.New("recurring occurrence has no original start")
	}
	start := *event.OriginalStart
	instant, err := start.Instant()
	if err != nil {
		return "", fmt.Errorf("invalid original start: %w", err)
	}
	canonical := "date:" + start.Date
	if !start.IsAllDay() {
		canonical = "instant:" + instant.UTC().Format(time.RFC3339Nano)
	}
	return mappingKey(sourceCalendarID, mappingKey("series:"+event.RecurringEventID, canonical)), nil
}
