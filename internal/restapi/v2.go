package restapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
)

type createEventV2Request struct {
	CalendarID         string                      `json:"calendar_id"`
	Event              calendar.EventCreateV2      `json:"event"`
	NotificationPolicy calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type updateEventV2Request struct {
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

type deleteEventV2Request struct {
	CalendarID         string                      `json:"calendar_id"`
	EventID            string                      `json:"event_id"`
	Scope              calendar.MutationScope      `json:"scope"`
	EffectiveFrom      *calendar.EventTime         `json:"effective_from,omitempty"`
	ExpectedETag       string                      `json:"expected_etag,omitempty"`
	OperationID        string                      `json:"operation_id,omitempty"`
	PreviewOnly        bool                        `json:"preview_only,omitempty"`
	NotificationPolicy calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type respondEventV2Request struct {
	CalendarID         string                      `json:"calendar_id"`
	EventID            string                      `json:"event_id"`
	Response           string                      `json:"response"`
	Comment            string                      `json:"comment,omitempty"`
	ExpectedETag       string                      `json:"expected_etag,omitempty"`
	NotificationPolicy calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type moveEventV2Request struct {
	CalendarID          string                      `json:"calendar_id"`
	EventID             string                      `json:"event_id"`
	DestinationCalendar string                      `json:"destination_calendar_id"`
	ExpectedETag        string                      `json:"expected_etag,omitempty"`
	NotificationPolicy  calendar.NotificationPolicy `json:"notification_policy,omitempty"`
}

type importEventV2Request struct {
	CalendarID string                 `json:"calendar_id"`
	Event      calendar.EventCreateV2 `json:"event"`
}

func registerV2Routes(mux *http.ServeMux, app *application.Service) {
	mux.HandleFunc("GET /api/v2/capabilities", func(w http.ResponseWriter, r *http.Request) {
		calendarID := r.URL.Query().Get("calendar_id")
		if calendarID == "" {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "calendar_id is required"))
			return
		}
		capabilities, err := app.Capabilities(r.Context(), calendarID)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, capabilities)
	})

	mux.HandleFunc("GET /api/v2/events", func(w http.ResponseWriter, r *http.Request) {
		calendarID := r.URL.Query().Get("calendar_id")
		eventID := r.URL.Query().Get("event_id")
		if eventID != "" {
			event, err := app.GetEvent(r.Context(), calendar.EventRef{CalendarID: calendarID, EventID: eventID})
			if err != nil {
				writeAPIError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, event)
			return
		}
		start, err := time.Parse(time.RFC3339, r.URL.Query().Get("start"))
		if err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid start: "+err.Error()))
			return
		}
		end, err := time.Parse(time.RFC3339, r.URL.Query().Get("end"))
		if err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid end: "+err.Error()))
			return
		}
		maxResults, err := parseMaxResults(r)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		page, err := app.ListEvents(r.Context(), calendar.ListEventsRequestV2{
			CalendarID: calendarID, Start: start, End: end,
			View:        calendar.RecurrenceView(r.URL.Query().Get("view")),
			ShowDeleted: r.URL.Query().Get("show_deleted") == "true",
			PageToken:   r.URL.Query().Get("page_token"),
			MaxResults:  maxResults,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})

	mux.HandleFunc("POST /api/v2/events", func(w http.ResponseWriter, r *http.Request) {
		var request createEventV2Request
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error()))
			return
		}
		event, err := app.CreateEvent(r.Context(), calendar.CreateEventRequestV2{
			CalendarID: request.CalendarID, Event: request.Event, Notifications: request.NotificationPolicy,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, event)
	})

	mux.HandleFunc("PATCH /api/v2/events", func(w http.ResponseWriter, r *http.Request) {
		var request updateEventV2Request
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error()))
			return
		}
		var patch calendar.EventPatchV2
		if err := json.Unmarshal(request.Patch, &patch); err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid patch: "+err.Error()))
			return
		}
		result, err := app.UpdateEvent(r.Context(), calendar.UpdateEventRequestV2{
			Ref:   calendar.EventRef{CalendarID: request.CalendarID, EventID: request.EventID},
			Patch: patch, Scope: request.Scope, EffectiveFrom: request.EffectiveFrom, ExpectedETag: request.ExpectedETag,
			OperationID: request.OperationID, PreviewOnly: request.PreviewOnly, Notifications: request.NotificationPolicy,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("DELETE /api/v2/events", func(w http.ResponseWriter, r *http.Request) {
		var request deleteEventV2Request
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error()))
			return
		}
		result, err := app.DeleteEvent(r.Context(), calendar.DeleteEventRequestV2{
			Ref: calendar.EventRef{CalendarID: request.CalendarID, EventID: request.EventID}, Scope: request.Scope,
			EffectiveFrom: request.EffectiveFrom, ExpectedETag: request.ExpectedETag, OperationID: request.OperationID,
			PreviewOnly: request.PreviewOnly, Notifications: request.NotificationPolicy,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("GET /api/v2/events/instances", func(w http.ResponseWriter, r *http.Request) {
		start, err := time.Parse(time.RFC3339, r.URL.Query().Get("start"))
		if err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid start: "+err.Error()))
			return
		}
		end, err := time.Parse(time.RFC3339, r.URL.Query().Get("end"))
		if err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid end: "+err.Error()))
			return
		}
		maxResults, err := parseMaxResults(r)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		page, err := app.GetEventInstances(r.Context(), calendar.InstancesRequestV2{
			Ref:   calendar.EventRef{CalendarID: r.URL.Query().Get("calendar_id"), EventID: r.URL.Query().Get("event_id")},
			Start: start, End: end, ShowDeleted: r.URL.Query().Get("show_deleted") == "true", PageToken: r.URL.Query().Get("page_token"), MaxResults: maxResults,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})

	mux.HandleFunc("GET /api/v2/events/search", func(w http.ResponseWriter, r *http.Request) {
		var start, end time.Time
		var err error
		if value := r.URL.Query().Get("start"); value != "" {
			start, err = time.Parse(time.RFC3339, value)
			if err != nil {
				writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid start: "+err.Error()))
				return
			}
		}
		if value := r.URL.Query().Get("end"); value != "" {
			end, err = time.Parse(time.RFC3339, value)
			if err != nil {
				writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid end: "+err.Error()))
				return
			}
		}
		maxResults, err := parseMaxResults(r)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		page, err := app.SearchEvents(r.Context(), calendar.SearchEventsRequestV2{
			CalendarID: r.URL.Query().Get("calendar_id"), Query: r.URL.Query().Get("query"), Start: start, End: end,
			EventTypes: r.URL.Query()["event_type"], ShowDeleted: r.URL.Query().Get("show_deleted") == "true", PageToken: r.URL.Query().Get("page_token"), MaxResults: maxResults,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})

	mux.HandleFunc("POST /api/v2/events/respond", func(w http.ResponseWriter, r *http.Request) {
		var request respondEventV2Request
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error()))
			return
		}
		result, err := app.RespondToEvent(r.Context(), calendar.RespondToEventRequestV2{
			Ref: calendar.EventRef{CalendarID: request.CalendarID, EventID: request.EventID}, Response: request.Response,
			Comment: request.Comment, ExpectedETag: request.ExpectedETag, Notifications: request.NotificationPolicy,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("POST /api/v2/events/move", func(w http.ResponseWriter, r *http.Request) {
		var request moveEventV2Request
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error()))
			return
		}
		result, err := app.MoveEvent(r.Context(), calendar.MoveEventRequestV2{
			Ref: calendar.EventRef{CalendarID: request.CalendarID, EventID: request.EventID}, DestinationCalendarID: request.DestinationCalendar,
			ExpectedETag: request.ExpectedETag, Notifications: request.NotificationPolicy,
		})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("POST /api/v2/events/import", func(w http.ResponseWriter, r *http.Request) {
		var request importEventV2Request
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, err.Error()))
			return
		}
		event, err := app.ImportEvent(r.Context(), calendar.ImportEventRequestV2{CalendarID: request.CalendarID, Event: request.Event})
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, event)
	})
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func parseMaxResults(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("max_results")
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, calendar.NewAPIError(calendar.ErrorInvalidArgument, "max_results must be a positive integer")
	}
	return parsed, nil
}

func writeAPIError(w http.ResponseWriter, err error) {
	apiErr := &calendar.APIError{}
	if !errors.As(err, &apiErr) {
		apiErr = &calendar.APIError{Code: calendar.ErrorProviderUnavailable, Message: err.Error(), Retryable: true, Cause: err}
	}
	status := http.StatusBadGateway
	switch apiErr.Code {
	case calendar.ErrorInvalidArgument, calendar.ErrorInvalidRecurrence:
		status = http.StatusBadRequest
	case calendar.ErrorUnsupportedCapability:
		status = http.StatusUnprocessableEntity
	case calendar.ErrorNotFound:
		status = http.StatusNotFound
	case calendar.ErrorPermissionDenied:
		status = http.StatusForbidden
	case calendar.ErrorConflict:
		status = http.StatusConflict
	case calendar.ErrorRateLimited:
		status = http.StatusTooManyRequests
	case calendar.ErrorProviderUnavailable, calendar.ErrorPartialFailure:
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"error": apiErr})
}
