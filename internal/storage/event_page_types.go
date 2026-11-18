package storage

import (
	"time"

	"calendar-mcp/internal/calendar"
)

// CachedPageRequest describes one bounded cached-event query.
type CachedPageRequest struct {
	CalendarIDs []string  `json:"calendar_ids"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	PageSize    int       `json:"page_size"`
}

// CachedPagePosition is the stable keyset position of a cached event row.
type CachedPagePosition struct {
	AllDay                                  bool
	StartAt, StartDate, EventID, CalendarID string
}

// CachedPageSummary is the public-safe event summary returned by cached pages.
type CachedPageSummary struct {
	Kind       string             `json:"kind"`
	EventID    string             `json:"event_id"`
	CalendarID string             `json:"calendar_id"`
	Title      string             `json:"title,omitempty"`
	Status     string             `json:"status,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Start      calendar.EventTime `json:"start"`
	End        calendar.EventTime `json:"end"`
}

type CachedPageRow struct {
	Summary  CachedPageSummary
	Position CachedPagePosition
}

type CachedPageSource struct {
	CalendarID    string     `json:"calendar_id"`
	Provider      string     `json:"provider"`
	Status        string     `json:"status"`
	Stale         bool       `json:"stale"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	NextSyncAt    *time.Time `json:"next_sync_at,omitempty"`
	WindowStart   time.Time  `json:"window_start"`
	WindowEnd     time.Time  `json:"window_end"`
}

type CachedPageReadStats struct {
	RowsDecoded     int   `json:"rows_decoded"`
	PayloadBytes    int64 `json:"payload_bytes"`
	MaxPayloadBytes int   `json:"max_payload_bytes"`
	PoolWaitNS      int64 `json:"pool_wait_ns"`
	SQLBeginNS      int64 `json:"sql_begin_ns"`
	SQLMetadataNS   int64 `json:"sql_metadata_ns"`
	SQLEventsNS     int64 `json:"sql_events_ns"`
	SQLCommitNS     int64 `json:"sql_commit_ns"`
	DecodeNS        int64 `json:"decode_ns"`
}

type CachedPageRead struct {
	Rows       []CachedPageRow
	Sources    []CachedPageSource
	Tickets    []AuthorityTicket
	ObservedAt time.Time
	Stats      CachedPageReadStats
}
