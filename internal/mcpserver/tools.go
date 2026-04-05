package mcpserver

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"calendar-mcp/internal/calendar"
)

func registerTools(s *server.MCPServer, reg *calendar.Registry) {
	s.AddTool(mcp.NewTool("list_calendars",
		mcp.WithDescription("List all calendars across Google, Microsoft 365, and Apple accounts. Returns calendar IDs needed for other operations."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cals, err := reg.ListCalendars(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(cals)
	})

	s.AddTool(mcp.NewTool("get_events",
		mcp.WithDescription("Get events from a specific calendar or all calendars within a date range."),
		mcp.WithString("calendar_id", mcp.Description("Calendar ID (e.g. google:primary). Omit to get events from all calendars.")),
		mcp.WithString("start", mcp.Required(), mcp.Description("Start datetime ISO8601 (e.g. 2026-04-05T00:00:00Z)")),
		mcp.WithString("end", mcp.Required(), mcp.Description("End datetime ISO8601 (e.g. 2026-04-06T00:00:00Z)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calID := req.GetString("calendar_id", "")
		startStr := req.GetString("start", "")
		endStr := req.GetString("end", "")

		start, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			return mcp.NewToolResultError("invalid start: " + err.Error()), nil
		}
		end, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			return mcp.NewToolResultError("invalid end: " + err.Error()), nil
		}

		events, err := reg.GetEvents(ctx, calID, start, end)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(events)
	})

	s.AddTool(mcp.NewTool("create_event",
		mcp.WithDescription("Create a new calendar event."),
		mcp.WithString("calendar_id", mcp.Required(), mcp.Description("Calendar ID to create event in (e.g. google:primary)")),
		mcp.WithString("title", mcp.Required(), mcp.Description("Event title")),
		mcp.WithString("start", mcp.Required(), mcp.Description("Start datetime ISO8601")),
		mcp.WithString("end", mcp.Required(), mcp.Description("End datetime ISO8601")),
		mcp.WithString("description", mcp.Description("Event description")),
		mcp.WithString("location", mcp.Description("Event location")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calID := req.GetString("calendar_id", "")
		title := req.GetString("title", "")
		startStr := req.GetString("start", "")
		endStr := req.GetString("end", "")

		start, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			return mcp.NewToolResultError("invalid start: " + err.Error()), nil
		}
		end, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			return mcp.NewToolResultError("invalid end: " + err.Error()), nil
		}

		ev, err := reg.CreateEvent(ctx, calID, calendar.EventCreate{
			Title:       title,
			Start:       start,
			End:         end,
			Description: req.GetString("description", ""),
			Location:    req.GetString("location", ""),
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(ev)
	})

	s.AddTool(mcp.NewTool("update_event",
		mcp.WithDescription("Update an existing calendar event. Only provided fields are changed."),
		mcp.WithString("calendar_id", mcp.Required(), mcp.Description("Calendar ID")),
		mcp.WithString("event_id", mcp.Required(), mcp.Description("Event ID to update")),
		mcp.WithString("title", mcp.Description("New title")),
		mcp.WithString("start", mcp.Description("New start datetime ISO8601")),
		mcp.WithString("end", mcp.Description("New end datetime ISO8601")),
		mcp.WithString("description", mcp.Description("New description")),
		mcp.WithString("location", mcp.Description("New location")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calID := req.GetString("calendar_id", "")
		eventID := req.GetString("event_id", "")

		upd := calendar.EventUpdate{}
		if v := req.GetString("title", ""); v != "" {
			upd.Title = &v
		}
		if v := req.GetString("description", ""); v != "" {
			upd.Description = &v
		}
		if v := req.GetString("location", ""); v != "" {
			upd.Location = &v
		}
		if v := req.GetString("start", ""); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return mcp.NewToolResultError("invalid start: " + err.Error()), nil
			}
			upd.Start = &t
		}
		if v := req.GetString("end", ""); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return mcp.NewToolResultError("invalid end: " + err.Error()), nil
			}
			upd.End = &t
		}

		ev, err := reg.UpdateEvent(ctx, calID, eventID, upd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(ev)
	})

	s.AddTool(mcp.NewTool("delete_event",
		mcp.WithDescription("Delete a calendar event."),
		mcp.WithString("calendar_id", mcp.Required(), mcp.Description("Calendar ID")),
		mcp.WithString("event_id", mcp.Required(), mcp.Description("Event ID to delete")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calID := req.GetString("calendar_id", "")
		eventID := req.GetString("event_id", "")

		if err := reg.DeleteEvent(ctx, calID, eventID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("event deleted"), nil
	})
}
