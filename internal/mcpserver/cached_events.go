package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/storage"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// An optional local diagnostics sink receives only bounded counters/timings.
// It has no request arguments, event data, credentials, or cursor contents.
type cachedResultObserver interface {
	ObserveCachedResult(cachedpages.PageStats, time.Duration, int)
}

var cachedEventsSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["calendar_ids","start","end"],
  "properties":{
    "calendar_ids":{"type":"array","minItems":1,"maxItems":16,"uniqueItems":true,"items":{"type":"string","maxLength":1024},"description":"Explicit canonical platform Google calendar IDs. Every source must have locally verified detail access and cached sync state."},
    "start":{"type":"string","format":"date-time"},
    "end":{"type":"string","format":"date-time"},
    "page_size":{"type":"integer","minimum":1,"maximum":50,"default":25},
    "page_token":{"type":"string","maxLength":4096}
  }
}`)

var coverageEventsSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["start","end"],
  "properties":{
    "calendar_ids":{"type":"array","minItems":1,"maxItems":16,"uniqueItems":true,"items":{"type":"string","maxLength":1024},"description":"Canonical platform calendar IDs. Omit to select all enabled readable sources, up to 16."},
    "start":{"type":"string","format":"date-time"},
    "end":{"type":"string","format":"date-time"},
    "page_size":{"type":"integer","minimum":1,"maximum":50,"default":25},
    "page_token":{"type":"string","maxLength":4096}
  }
}`)

type coverageEventReader interface {
	ReadCoverage(context.Context, cachedpages.Request, string) (cachedpages.Page, cachedpages.PageStats, error)
}

func registerCoverageEvents(s *server.MCPServer, reader CachedEventReader, coverage coverageEventReader) {
	tool := mcp.NewToolWithRawSchema("get_events", "List compact cached event summaries across enabled calendars. Follow next_page_token with the same filters. The last page marks complete only if all selected ranges are covered without known omissions. Consult sources for coverage and last successful sync, and use get_event for details or post-write checks. No provider calls or sync are started.", coverageEventsSchema)
	mcp.WithReadOnlyHintAnnotation(true)(&tool)
	mcp.WithDestructiveHintAnnotation(false)(&tool)
	mcp.WithOpenWorldHintAnnotation(false)(&tool)
	registerBoundedEvents(s, tool, reader, coverage.ReadCoverage, true)
}

func registerCachedEvents(s *server.MCPServer, reader CachedEventReader) {
	tool := mcp.NewToolWithRawSchema("get_cached_events", "Read bounded cached Google event summaries for 1–16 explicit canonical calendar IDs and a time range within the configured cache window. No provider calls or sync are started. Results use cached_weak consistency; sources report freshness and target windows, not verified provider coverage. Complete means the end of the current cached selection. page_size is an upper bound; the complete SDK result is limited to 16 KiB. Oversized summaries produce explicit partial markers. Continue with next_page_token only while more information is needed. Tokens expire after 300 seconds and bind the caller, filters, server configuration and local rights; an unavailable token must not silently restart the query.", cachedEventsSchema)
	registerBoundedEvents(s, tool, reader, reader.Read, false)
}

func registerBoundedEvents(s *server.MCPServer, tool mcp.Tool, reader CachedEventReader, read func(context.Context, cachedpages.Request, string) (cachedpages.Page, cachedpages.PageStats, error), optionalScope bool) {
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := cachedArgumentsJSON(ctx, request)
		if err != nil {
			return cachedError("invalid_argument"), nil
		}
		args, token, err := parseCachedArgumentsForCatalog(raw, optionalScope)
		if err != nil {
			return cachedError("invalid_argument"), nil
		}
		page, stats, err := read(ctx, args, token)
		if err != nil {
			switch {
			case errors.Is(err, storage.ErrExplicitCachedPageScope):
				return mcp.NewToolResultError("scope_required: select at most 16 explicit calendar IDs"), nil
			case errors.Is(err, cachedpages.ErrInvalidArgument):
				return cachedError("invalid_argument"), nil
			case errors.Is(err, cachedpages.ErrCursorUnavailable):
				return cachedError("cursor_unavailable"), nil
			default:
				return cachedError("unavailable"), nil
			}
		}
		started := time.Now()
		result, bytes, err := cachedpages.NewToolResult(page)
		elapsed := time.Since(started)
		if err != nil || bytes > cachedpages.MaxResultBytes {
			return cachedError("unavailable"), nil
		}
		if observer, ok := reader.(cachedResultObserver); ok {
			observer.ObserveCachedResult(stats, elapsed, bytes)
		}
		return result, nil
	})
}

func cachedError(code string) *mcp.CallToolResult {
	return mcp.NewToolResultError(code + ": cached event request could not be completed")
}
