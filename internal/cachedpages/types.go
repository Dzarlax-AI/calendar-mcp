// Package cachedpages contains bounded DTOs, cursor encoding, and page packing
// for the cached-event reader.
package cachedpages

import (
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

const (
	MaxResultBytes  = 16 << 10
	MaxPayloadBytes = 32 << 10
	MaxTokenBytes   = 4 << 10
)

type Request = storage.CachedPageRequest
type Position = storage.CachedPagePosition
type Summary = storage.CachedPageSummary
type Row = storage.CachedPageRow
type Source = storage.CachedPageSource
type ReadPage = storage.CachedPageRead
type ReadStats = storage.CachedPageReadStats

// Page is the stable public representation of one cached-event page.
type Page struct {
	Items         []Summary `json:"items"`
	NextPageToken string    `json:"next_page_token,omitempty"`
	Sources       []Source  `json:"sources"`
	Consistency   string    `json:"consistency"`
	ObservedAt    time.Time `json:"observed_at"`
	Complete      bool      `json:"complete"`
	Partial       bool      `json:"partial"`
}

// MarkerSummary creates the bounded public marker for a summary that cannot be
// returned whole within the wire budget. The event's identifying range and
// state are retained so the marker remains useful to callers.
func MarkerSummary(summary Summary) Summary {
	return Summary{
		Kind:       "oversized",
		EventID:    summary.EventID,
		CalendarID: summary.CalendarID,
		Start:      summary.Start,
		End:        summary.End,
		Status:     summary.Status,
		Reason:     "result_size_limit",
	}
}

// EventTime is re-exported for callers assembling summaries without importing
// the lower-level calendar package separately.
type EventTime = calendar.EventTime
