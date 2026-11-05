package syncengine

import (
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
	"time"
)

type cacheCopyCandidate = calendarprojection.Candidate

func cacheCopyCandidates(source string, events []calendar.EventV2, start, end time.Time, zone *time.Location) ([]cacheCopyCandidate, error) {
	return calendarprojection.Candidates(source, events, start, end, zone)
}
