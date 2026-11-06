package syncengine

import (
	"errors"
	"fmt"
	"strings"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
)

// cacheCopyIdentity is kept for the sync engine's existing callers.
func cacheCopyIdentity(sourceCalendarID string, event calendar.EventV2) (string, error) {
	return calendarprojection.Identity(sourceCalendarID, event)
}

// cacheCopyCreate builds a standalone target event with the same canonical
// identity in its ownership marker and UID. It performs no provider calls.
func cacheCopyCreate(ruleID, sourceCalendarID string, event calendar.EventV2, targetProvider string) (calendar.EventCreateV2, error) {
	if ruleID == "" || strings.ContainsRune(ruleID, '\x00') {
		return calendar.EventCreateV2{}, errors.New("rule ID is required")
	}
	identity, err := cacheCopyIdentity(sourceCalendarID, event)
	if err != nil {
		return calendar.EventCreateV2{}, err
	}
	if event.Status == "cancelled" {
		return calendar.EventCreateV2{}, errors.New("cancelled events cannot be created")
	}
	if err := calendar.ValidateEventTimeRangeV2(event.Start, event.End); err != nil {
		return calendar.EventCreateV2{}, fmt.Errorf("invalid source event time: %w", err)
	}
	marker := newMappingID(ruleID, identity, "")
	created := mirrorCreate(ruleID, event, targetProvider)
	created.ICalUID = marker + "@calendar-mcp"
	if targetProvider == "google" {
		created.ICalUID = ""
	}
	created.Recurrence = nil
	// Copies must never invite the source event participants.
	created.Attendees = nil
	created.SyncMarker = &calendar.SyncMarker{RuleID: ruleID, SourceEventID: marker}
	if targetProvider == "google" {
		created.Google = &calendar.GoogleEventExtension{PrivateProperties: map[string]string{"calendar_sync_rule": ruleID, "calendar_source_event": marker}}
	}
	return created, nil
}

// cacheCopyScope addresses the standalone target object, not the source
// instance's recurring-series scope.
func cacheCopyScope() calendar.MutationScope { return calendar.ScopeSeries }
