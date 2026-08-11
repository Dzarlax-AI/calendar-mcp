package mcpserver

import (
	"context"
	"encoding/json"
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
		mcp.WithString("start", mcp.Required(), mcp.Description("Start datetime ISO8601 or all-day date (YYYY-MM-DD)")),
		mcp.WithString("end", mcp.Required(), mcp.Description("End datetime ISO8601 or all-day exclusive date (YYYY-MM-DD)")),
		mcp.WithString("description", mcp.Description("Event description")),
		mcp.WithString("location", mcp.Description("Event location")),
		mcp.WithString("attendees", mcp.Description("JSON array of attendees: [{\"email\":\"a@b.com\",\"name\":\"Name\",\"optional\":false}]")),
		mcp.WithBoolean("video_call", mcp.Description("Auto-create Google Meet or MS Teams link")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calID := req.GetString("calendar_id", "")
		title := req.GetString("title", "")
		startStr := req.GetString("start", "")
		endStr := req.GetString("end", "")

		start, end, allDay, err := calendar.ParseEventTimeRange(startStr, endStr)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		var attendees []calendar.Attendee
		if raw := req.GetString("attendees", ""); raw != "" {
			if err := json.Unmarshal([]byte(raw), &attendees); err != nil {
				return mcp.NewToolResultError("invalid attendees JSON: " + err.Error()), nil
			}
		}

		ev, err := reg.CreateEvent(ctx, calID, calendar.EventCreate{
			Title:       title,
			Start:       start,
			End:         end,
			AllDay:      allDay,
			Description: req.GetString("description", ""),
			Location:    req.GetString("location", ""),
			Attendees:   attendees,
			VideoCall:   req.GetBool("video_call", false),
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
		mcp.WithString("start", mcp.Description("New start datetime ISO8601 or all-day date (YYYY-MM-DD)")),
		mcp.WithString("end", mcp.Description("New end datetime ISO8601 or all-day exclusive date (YYYY-MM-DD)")),
		mcp.WithString("description", mcp.Description("New description")),
		mcp.WithString("location", mcp.Description("New location")),
		mcp.WithString("attendees", mcp.Description("JSON array of attendees (replaces existing): [{\"email\":\"a@b.com\",\"name\":\"Name\"}]")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calID := req.GetString("calendar_id", "")
		eventID := req.GetString("event_id", "")
		args := req.GetArguments()

		upd := calendar.EventUpdate{}
		if _, ok := args["title"]; ok {
			v := req.GetString("title", "")
			upd.Title = &v
		}
		if _, ok := args["description"]; ok {
			v := req.GetString("description", "")
			upd.Description = &v
		}
		if _, ok := args["location"]; ok {
			v := req.GetString("location", "")
			upd.Location = &v
		}
		startValue, hasStart := args["start"]
		endValue, hasEnd := args["end"]
		if hasStart != hasEnd {
			return mcp.NewToolResultError("start and end must be provided together"), nil
		}
		if hasStart {
			v, _ := startValue.(string)
			parsed, err := calendar.ParseEventTime(v)
			if err != nil {
				return mcp.NewToolResultError("invalid start: " + err.Error()), nil
			}
			upd.Start = &parsed.Time
			upd.AllDay, err = calendar.MergeOptionalAllDay(upd.AllDay, parsed)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}
		if hasEnd {
			v, _ := endValue.(string)
			parsed, err := calendar.ParseEventTime(v)
			if err != nil {
				return mcp.NewToolResultError("invalid end: " + err.Error()), nil
			}
			upd.End = &parsed.Time
			upd.AllDay, err = calendar.MergeOptionalAllDay(upd.AllDay, parsed)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}
		if upd.Start != nil && upd.End != nil {
			if err := calendar.ValidateEventTimeRange(*upd.Start, *upd.End); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}

		if _, ok := args["attendees"]; ok {
			raw := req.GetString("attendees", "")
			var attendees []calendar.Attendee
			if raw != "" {
				if err := json.Unmarshal([]byte(raw), &attendees); err != nil {
					return mcp.NewToolResultError("invalid attendees JSON: " + err.Error()), nil
				}
			}
			upd.Attendees = &attendees
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
