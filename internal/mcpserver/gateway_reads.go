package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type gatewayCatalogReader interface {
	CachedEventReader
	GatewayListCalendars(context.Context) ([]calendar.Calendar, error)
	GatewayCapabilities(context.Context, string) (calendar.CalendarCapabilities, error)
	GatewayCoverage(context.Context, cachedpages.Request, string) (cachedpages.Page, cachedpages.PageStats, error)
	GatewayEvent(context.Context, calendar.EventRef) (calendar.EventV2, error)
	GatewayDetails(context.Context, string, cachedpages.GatewayDetailsRequest) (cachedpages.GatewayDetailsPage, error)
}

func registerGatewayReads(s *server.MCPServer, reader gatewayCatalogReader) {
	// Preserve the registered tool schemas when replacing the read handlers.
	replace := func(name, description string, fn server.ToolHandlerFunc) {
		tool := s.GetTool(name).Tool
		if description != "" {
			tool.Description = description
		}
		s.AddTool(tool, fn)
	}
	s.AddTool(mcp.NewTool("list_calendars", mcp.WithDescription("List calendars from the locally verified calendar inventory.")), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		items, err := reader.GatewayListCalendars(ctx)
		if err != nil {
			return gatewayReadError(err), nil
		}
		result, err := jsonResult(items)
		if err != nil {
			return nil, err
		}
		if raw, e := json.Marshal(result); e != nil || len(raw) > cachedpages.MaxResultBytes {
			return gatewayReadError(cachedpages.ErrGatewayResultTooLarge), nil
		}
		return result, nil
	})
	replace("get_calendar_capabilities", "Read calendar capabilities and effective permissions from the local snapshot.", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args capabilitiesArgs
		if req.BindArguments(&args) != nil {
			return gatewayReadError(cachedpages.ErrInvalidArgument), nil
		}
		out, err := reader.GatewayCapabilities(ctx, args.CalendarID)
		return gatewayReadResult(out, err), nil
	})
	replace("get_event", "Read locally published details for one event.", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args getEventV2Args
		if req.BindArguments(&args) != nil {
			return gatewayReadError(cachedpages.ErrInvalidArgument), nil
		}
		out, err := reader.GatewayEvent(ctx, calendar.EventRef{CalendarID: args.CalendarID, EventID: args.EventID})
		return gatewayReadResult(out, err), nil
	})
	replace("get_event_instances", "List locally published occurrences of a recurring series in a bounded time range.", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args instancesV2Args
		if req.BindArguments(&args) != nil {
			return gatewayReadError(cachedpages.ErrInvalidArgument), nil
		}
		start, e1 := time.Parse(time.RFC3339, args.Start)
		end, e2 := time.Parse(time.RFC3339, args.End)
		if e1 != nil || e2 != nil {
			return gatewayReadError(cachedpages.ErrInvalidArgument), nil
		}
		out, err := reader.GatewayDetails(ctx, "get_event_instances", cachedpages.GatewayDetailsRequest{CalendarID: args.CalendarID, EventID: args.EventID,
			Start: start, End: end, ShowDeleted: args.ShowDeleted, MaxResults: args.MaxResults, PageToken: args.PageToken})
		return gatewayReadResult(out, err), nil
	})
	replace("search_events", "Search the local cache for a case-insensitive substring in authorized title, description, or location. Omitted time bounds use the cache window.", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args searchEventsV2Args
		if req.BindArguments(&args) != nil {
			return gatewayReadError(cachedpages.ErrInvalidArgument), nil
		}
		start, e1 := optionalGatewayBound(args.Start)
		end, e2 := optionalGatewayBound(args.End)
		if e1 != nil || e2 != nil {
			return gatewayReadError(cachedpages.ErrInvalidArgument), nil
		}
		out, err := reader.GatewayDetails(ctx, "search_events", cachedpages.GatewayDetailsRequest{CalendarID: args.CalendarID,
			Query: args.Query, Start: start, End: end, EventTypes: args.EventTypes, ShowDeleted: args.ShowDeleted, MaxResults: args.MaxResults, PageToken: args.PageToken})
		return gatewayReadResult(out, err), nil
	})
	tool := mcp.NewToolWithRawSchema("get_events", "List compact local event summaries and coverage. Follow the continuation with the same filters.", coverageEventsSchema)
	mcp.WithReadOnlyHintAnnotation(true)(&tool)
	mcp.WithDestructiveHintAnnotation(false)(&tool)
	mcp.WithOpenWorldHintAnnotation(false)(&tool)
	registerBoundedEvents(s, tool, reader, reader.GatewayCoverage, true)
}

func optionalGatewayBound(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func gatewayReadResult(value any, err error) *mcp.CallToolResult {
	if err != nil {
		return gatewayReadError(err)
	}
	if !cachedpages.GatewayResultFits(value) {
		return gatewayReadError(cachedpages.ErrGatewayResultTooLarge)
	}
	return v2Result(value)
}

func gatewayReadError(err error) *mcp.CallToolResult {
	var code calendar.ErrorCode
	var msg string
	switch {
	case errors.Is(err, cachedpages.ErrInvalidArgument):
		code, msg = calendar.ErrorInvalidArgument, "Invalid cached read arguments."
	case errors.Is(err, cachedpages.ErrCursorUnavailable):
		code, msg = "cursor_unavailable", "Start again from the first page."
	case errors.Is(err, cachedpages.ErrGatewayNotFound):
		code, msg = calendar.ErrorNotFound, "Event deletion is confirmed."
	case errors.Is(err, cachedpages.ErrGatewayCachePending):
		code, msg = "cache_pending", "Calendar changes are awaiting local publication."
	case errors.Is(err, cachedpages.ErrGatewayCacheMiss):
		code, msg = "cache_miss", "The event has no confirmed cached result."
	case errors.Is(err, storage.ErrExplicitCachedPageScope):
		code, msg = "scope_required", "Select at most 16 explicit calendar IDs."
	case errors.Is(err, cachedpages.ErrGatewayResultTooLarge) || errors.Is(err, storage.ErrGatewayQueryTooLarge):
		code, msg = "result_too_large", "The cached result exceeds the size limit."
	default:
		code, msg = "unavailable", "Current local data or permissions are unavailable."
	}
	return v2Error(calendar.NewAPIError(code, msg))
}

func registerGatewayWriteTracking(s *server.MCPServer) {
	for _, name := range []string{"create_event", "update_event", "delete_event", "respond_to_event", "move_event", "import_event"} {
		current := s.GetTool(name)
		s.AddTool(current.Tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Bound the request well before the durable outcome deadline, including
			// multi-step writes. A timeout can still leave the provider outcome unknown.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			ctx, tracking := calendar.EnableGatewayWriteTracking(ctx)
			result, err := current.Handler(ctx, request)
			if err != nil || result == nil || result.IsError || !tracking.PendingPublication() {
				return result, err
			}
			switch value := result.StructuredContent.(type) {
			case *calendar.OperationResult:
				copy := *value
				copy.Warnings = append(slices.Clone(value.Warnings), calendar.GatewayWritePendingWarning)
				updated := v2Result(&copy)
				updated.Meta = result.Meta
				return updated, nil
			case *calendar.EventV2:
				meta := &mcp.Meta{AdditionalFields: map[string]any{}}
				if result.Meta != nil {
					meta.ProgressToken = result.Meta.ProgressToken
					maps.Copy(meta.AdditionalFields, result.Meta.AdditionalFields)
				}
				meta.AdditionalFields["calendar/cache_publication"] = "pending"
				meta.AdditionalFields["calendar/warnings"] = []string{calendar.GatewayWritePendingWarning}
				result.Meta = meta
			}
			return result, nil
		})
	}
}
