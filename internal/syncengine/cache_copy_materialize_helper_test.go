package syncengine

import (
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
	"time"
)

// Compatibility wrapper for synthetic tests; runtime callers must supply their
// actual frozen window directly to calendarprojection.Materialize.
func cacheCopyMaterialize(source, timezone string, mode calendar.EventSyncMode, base []calendar.EventV2, baseAvailable bool, pages []calendar.EventSyncPage) ([]calendar.EventV2, []calendar.EventSyncRemoval, error) {
	events, observations, err := calendarprojection.Materialize(source, timezone, time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), mode, base, baseAvailable, pages)
	if err != nil {
		return nil, nil, err
	}
	removals := make([]calendar.EventSyncRemoval, len(observations))
	for i, observation := range observations {
		removals[i] = observation.Removal
	}
	return events, removals, nil
}
