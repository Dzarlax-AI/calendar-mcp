package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"calendar-mcp/internal/cachedpages"
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

func registerCachedEvents(s *server.MCPServer, reader CachedEventReader) {
	tool := mcp.NewToolWithRawSchema("get_cached_events", "Read bounded cached Google event summaries for 1–16 explicit canonical calendar IDs and a time range within the configured cache window. No provider calls or sync are started. Results use cached_weak consistency; sources report freshness and target windows, not verified provider coverage. Complete means the end of the current cached selection. page_size is an upper bound; the complete SDK result is limited to 16 KiB. Oversized summaries produce explicit partial markers. Continue with next_page_token only while more information is needed. Tokens expire after 300 seconds and bind the caller, filters, server configuration and local rights; an unavailable token must not silently restart the query.", cachedEventsSchema)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := cachedArgumentsJSON(ctx, request)
		if err != nil {
			return cachedError("invalid_argument"), nil
		}
		args, token, err := parseCachedArguments(raw)
		if err != nil {
			return cachedError("invalid_argument"), nil
		}
		page, stats, err := reader.Read(ctx, args, token)
		if err != nil {
			switch {
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
