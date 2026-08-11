package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
)

type capabilitiesArgs struct {
	CalendarID string `json:"calendar_id"`
}

type getEventV2Args struct {
	CalendarID string `json:"calendar_id"`
	EventID    string `json:"event_id"`
}

type listEventsV2Args struct {
	CalendarID  string                  `json:"calendar_id,omitempty"`
	Start       string                  `json:"start"`
	End         string                  `json:"end"`
	View        calendar.RecurrenceView `json:"view,omitempty"`
	ShowDeleted bool                    `json:"show_deleted,omitempty"`
	PageToken   string                  `json:"page_token,omitempty"`
	MaxResults  int64                   `json:"max_results,omitempty"`
}

type createEventV2Args struct {
	CalendarID         string                      `json:"calendar_id"`
	Event              calendar.EventCreateV2      `json:"event"`
	NotificationPolicy calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type updateEventV2Args struct {
	CalendarID         string                      `json:"calendar_id"`
	EventID            string                      `json:"event_id"`
	Patch              json.RawMessage             `json:"patch"`
	Scope              calendar.MutationScope      `json:"scope"`
	EffectiveFrom      *calendar.EventTime         `json:"effective_from,omitempty"`
	ExpectedETag       string                      `json:"expected_etag,omitempty"`
	OperationID        string                      `json:"operation_id,omitempty"`
	PreviewOnly        bool                        `json:"preview_only,omitempty"`
	NotificationPolicy calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type deleteEventV2Args struct {
	CalendarID         string                      `json:"calendar_id"`
	EventID            string                      `json:"event_id"`
	Scope              calendar.MutationScope      `json:"scope"`
	EffectiveFrom      *calendar.EventTime         `json:"effective_from,omitempty"`
	ExpectedETag       string                      `json:"expected_etag,omitempty"`
	OperationID        string                      `json:"operation_id,omitempty"`
	PreviewOnly        bool                        `json:"preview_only,omitempty"`
	NotificationPolicy calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type instancesV2Args struct {
	CalendarID  string `json:"calendar_id"`
	EventID     string `json:"event_id"`
	Start       string `json:"start"`
	End         string `json:"end"`
	ShowDeleted bool   `json:"show_deleted,omitempty"`
	PageToken   string `json:"page_token,omitempty"`
	MaxResults  int64  `json:"max_results,omitempty"`
}

type searchEventsV2Args struct {
	CalendarID  string   `json:"calendar_id,omitempty"`
	Query       string   `json:"query"`
	Start       string   `json:"start,omitempty"`
	End         string   `json:"end,omitempty"`
	EventTypes  []string `json:"event_types,omitempty"`
	ShowDeleted bool     `json:"show_deleted,omitempty"`
	PageToken   string   `json:"page_token,omitempty"`
	MaxResults  int64    `json:"max_results,omitempty"`
}

type respondEventV2Args struct {
	CalendarID         string                      `json:"calendar_id"`
	EventID            string                      `json:"event_id"`
	Response           string                      `json:"response"`
	Comment            string                      `json:"comment,omitempty"`
	ExpectedETag       string                      `json:"expected_etag,omitempty"`
	NotificationPolicy calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type moveEventV2Args struct {
	CalendarID          string                      `json:"calendar_id"`
	EventID             string                      `json:"event_id"`
	DestinationCalendar string                      `json:"destination_calendar_id"`
	ExpectedETag        string                      `json:"expected_etag,omitempty"`
	NotificationPolicy  calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type importEventV2Args struct {
	CalendarID string                 `json:"calendar_id"`
	Event      calendar.EventCreateV2 `json:"event"`
}

var updateEventV2Schema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "required":["calendar_id","event_id","patch","scope"],
  "properties":{
    "calendar_id":{"type":"string"},
    "event_id":{"type":"string"},
    "patch":{"type":"object","description":"Fields to change. Empty strings and arrays clear values; omitted fields are preserved."},
    "scope":{"type":"string","enum":["series","single","following"]},
    "effective_from":{"type":"object"},
    "expected_etag":{"type":"string"},
    "operation_id":{"type":"string"},
    "preview_only":{"type":"boolean"},
    "notification_policy":{"type":"string","enum":["none","external_only","all"],"default":"none"}
  }
}`)

func registerToolsV2(s *server.MCPServer, app *application.Service) {
	s.AddTool(mcp.NewTool("get_calendar_capabilities",
		mcp.WithDescription("Return the operations, fields, mutation scopes, and notification policies supported by a calendar."),
		mcp.WithInputSchema[capabilitiesArgs](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args capabilitiesArgs
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		capabilities, err := app.Capabilities(ctx, args.CalendarID)
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(capabilities), nil
	})

	s.AddTool(mcp.NewTool("get_events_v2",
		mcp.WithDescription("List events with explicit recurrence view, pagination metadata, and per-source completeness."),
		mcp.WithInputSchema[listEventsV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args listEventsV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		start, err := time.Parse(time.RFC3339, args.Start)
		if err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid start: "+err.Error())), nil
		}
		end, err := time.Parse(time.RFC3339, args.End)
		if err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid end: "+err.Error())), nil
		}
		page, err := app.ListEvents(ctx, calendar.ListEventsRequestV2{
			CalendarID: args.CalendarID, Start: start, End: end, View: args.View,
			ShowDeleted: args.ShowDeleted, PageToken: args.PageToken, MaxResults: args.MaxResults,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(page), nil
	})

	s.AddTool(mcp.NewTool("get_event_v2",
		mcp.WithDescription("Get one event by its provider-prefixed calendar and event IDs."),
		mcp.WithInputSchema[getEventV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args getEventV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		event, err := app.GetEvent(ctx, calendar.EventRef{CalendarID: args.CalendarID, EventID: args.EventID})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(event), nil
	})

	s.AddTool(mcp.NewTool("create_event_v2",
		mcp.WithDescription("Create an event using typed time, recurrence, attendee, reminder, attachment, conference, and provider-extension fields."),
		mcp.WithInputSchema[createEventV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args createEventV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		event, err := app.CreateEvent(ctx, calendar.CreateEventRequestV2{
			CalendarID: args.CalendarID, Event: args.Event, Notifications: args.NotificationPolicy,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(event), nil
	})

	updateTool := mcp.NewToolWithRawSchema("update_event_v2", "Update a series, one occurrence, or this-and-following using a presence-aware patch.", updateEventV2Schema)
	s.AddTool(updateTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args updateEventV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		var patch calendar.EventPatchV2
		if err := json.Unmarshal(args.Patch, &patch); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid patch: "+err.Error())), nil
		}
		result, err := app.UpdateEvent(ctx, calendar.UpdateEventRequestV2{
			Ref:   calendar.EventRef{CalendarID: args.CalendarID, EventID: args.EventID},
			Patch: patch, Scope: args.Scope, EffectiveFrom: args.EffectiveFrom, ExpectedETag: args.ExpectedETag,
			OperationID: args.OperationID, PreviewOnly: args.PreviewOnly, Notifications: args.NotificationPolicy,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(result), nil
	})

	s.AddTool(mcp.NewTool("delete_event_v2",
		mcp.WithDescription("Delete a series, one occurrence, or this-and-following with explicit notification policy."),
		mcp.WithInputSchema[deleteEventV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args deleteEventV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		result, err := app.DeleteEvent(ctx, calendar.DeleteEventRequestV2{
			Ref: calendar.EventRef{CalendarID: args.CalendarID, EventID: args.EventID}, Scope: args.Scope,
			EffectiveFrom: args.EffectiveFrom, ExpectedETag: args.ExpectedETag, OperationID: args.OperationID,
			PreviewOnly: args.PreviewOnly, Notifications: args.NotificationPolicy,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(result), nil
	})

	s.AddTool(mcp.NewTool("get_event_instances_v2",
		mcp.WithDescription("List the concrete occurrences of a recurring series in a bounded time range."),
		mcp.WithInputSchema[instancesV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args instancesV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		start, err := time.Parse(time.RFC3339, args.Start)
		if err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid start: "+err.Error())), nil
		}
		end, err := time.Parse(time.RFC3339, args.End)
		if err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid end: "+err.Error())), nil
		}
		page, err := app.GetEventInstances(ctx, calendar.InstancesRequestV2{
			Ref: calendar.EventRef{CalendarID: args.CalendarID, EventID: args.EventID}, Start: start, End: end,
			ShowDeleted: args.ShowDeleted, PageToken: args.PageToken, MaxResults: args.MaxResults,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(page), nil
	})

	s.AddTool(mcp.NewTool("search_events_v2",
		mcp.WithDescription("Search events in one calendar or across all configured providers, with optional time and event-type filters."),
		mcp.WithInputSchema[searchEventsV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args searchEventsV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		var start, end time.Time
		var err error
		if args.Start != "" {
			start, err = time.Parse(time.RFC3339, args.Start)
			if err != nil {
				return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid start: "+err.Error())), nil
			}
		}
		if args.End != "" {
			end, err = time.Parse(time.RFC3339, args.End)
			if err != nil {
				return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid end: "+err.Error())), nil
			}
		}
		page, err := app.SearchEvents(ctx, calendar.SearchEventsRequestV2{
			CalendarID: args.CalendarID, Query: args.Query, Start: start, End: end, EventTypes: args.EventTypes,
			ShowDeleted: args.ShowDeleted, PageToken: args.PageToken, MaxResults: args.MaxResults,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(page), nil
	})

	s.AddTool(mcp.NewTool("respond_to_event_v2",
		mcp.WithDescription("Respond to an event invitation as the authenticated calendar user."),
		mcp.WithInputSchema[respondEventV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args respondEventV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		result, err := app.RespondToEvent(ctx, calendar.RespondToEventRequestV2{
			Ref: calendar.EventRef{CalendarID: args.CalendarID, EventID: args.EventID}, Response: args.Response,
			Comment: args.Comment, ExpectedETag: args.ExpectedETag, Notifications: args.NotificationPolicy,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(result), nil
	})

	s.AddTool(mcp.NewTool("move_event_v2",
		mcp.WithDescription("Move a supported event to another calendar within the same provider."),
		mcp.WithInputSchema[moveEventV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args moveEventV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		result, err := app.MoveEvent(ctx, calendar.MoveEventRequestV2{
			Ref: calendar.EventRef{CalendarID: args.CalendarID, EventID: args.EventID}, DestinationCalendarID: args.DestinationCalendar,
			ExpectedETag: args.ExpectedETag, Notifications: args.NotificationPolicy,
		})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(result), nil
	})

	s.AddTool(mcp.NewTool("import_event_v2",
		mcp.WithDescription("Import an externally identified event. Google requires event.ical_uid and supports only default events."),
		mcp.WithInputSchema[importEventV2Args](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args importEventV2Args
		if err := req.BindArguments(&args); err != nil {
			return v2Error(calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error())), nil
		}
		event, err := app.ImportEvent(ctx, calendar.ImportEventRequestV2{CalendarID: args.CalendarID, Event: args.Event})
		if err != nil {
			return v2Error(err), nil
		}
		return v2Result(event), nil
	})
}

func v2Result(value any) *mcp.CallToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		result := mcp.NewToolResultError("encode structured result: " + err.Error())
		result.IsError = true
		return result
	}
	return mcp.NewToolResultStructured(value, string(data))
}

func v2Error(err error) *mcp.CallToolResult {
	var structured any = calendar.NewAPIError(calendar.ErrorProviderUnavailable, err.Error())
	var apiErr *calendar.APIError
	if errors.As(err, &apiErr) {
		structured = apiErr
	}
	result := mcp.NewToolResultStructured(structured, err.Error())
	result.IsError = true
	return result
}
