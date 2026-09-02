// Package nativeapi exposes the narrow, token-authenticated API used by
// calendar-app. It has no provider setup and only supports deliberate ordinary
// event creates and edits with provider notifications forced off.
package nativeapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type Config struct {
	App             *application.Service
	CachedEventsApp *application.Service
	Store           *storage.Store
	Token           string // NATIVE_APP_TOKEN
	ReadOnlyToken   string // READ_ONLY_TOKEN
	WritesEnabled   bool
}

type Server struct {
	app             *application.Service
	cachedEventsApp *application.Service
	store           *storage.Store
	token           string
	readOnlyToken   string
	writesEnabled   bool
}

func New(cfg Config) *Server {
	return &Server{app: cfg.App, cachedEventsApp: cfg.CachedEventsApp, store: cfg.Store, token: cfg.Token, readOnlyToken: cfg.ReadOnlyToken, writesEnabled: cfg.WritesEnabled}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.token != "" {
		mux.Handle("GET /bootstrap", s.authorized(s.token, http.HandlerFunc(s.bootstrap)))
		mux.Handle("GET /events", s.authorized(s.token, http.HandlerFunc(s.events)))
		if s.writesEnabled {
			mux.Handle("POST /events", s.authorized(s.token, http.HandlerFunc(s.createEvent)))
			mux.Handle("PATCH /events/{calendar_id}/{event_id}", s.authorized(s.token, http.HandlerFunc(s.updateEvent)))
		}
	}
	if s.readOnlyToken != "" {
		mux.Handle("GET /cached-events", s.authorized(s.readOnlyToken, http.HandlerFunc(s.cachedEvents)))
	}
	return mux
}

// nativeEventTime is intentionally narrower than calendar.EventTime. The
// dashboard only needs a provider-normalized all-day date or RFC3339 instant;
// exposing a timezone or other provider metadata would not change rendering.
type nativeEventTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"date_time,omitempty"`
}

type nativeEvent struct {
	CalendarID string          `json:"calendar_id"`
	Title      string          `json:"title,omitempty"`
	Status     string          `json:"status,omitempty"`
	Start      nativeEventTime `json:"start"`
	End        nativeEventTime `json:"end"`
}

type cachedSource struct {
	CalendarID string `json:"calendar_id"`
	Status     string `json:"status"`
	Stale      bool   `json:"stale"`
}

type cachedEventsResponse struct {
	Items    []nativeEvent  `json:"items"`
	Sources  []cachedSource `json:"sources"`
	Complete bool           `json:"complete"`
}

func (s *Server) authorized(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type calendarResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TimeZone string `json:"time_zone,omitempty"`
	Color    string `json:"color,omitempty"`
	CanRead  bool   `json:"can_read"`
	CanWrite bool   `json:"can_write"`
	ReadOnly bool   `json:"read_only"`
}

func (s *Server) calendars(r *http.Request) ([]calendarResponse, error) {
	items, err := s.store.ListAllCalendars(r.Context())
	if err != nil {
		return nil, err
	}
	result := make([]calendarResponse, 0, len(items))
	for _, item := range items {
		if !item.CanRead {
			continue
		}
		canWrite := s.writesEnabled && item.CanWrite
		result = append(result, calendarResponse{ID: item.ID, Name: item.Name, TimeZone: item.Timezone, CanRead: true, CanWrite: canWrite, ReadOnly: !canWrite})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

type createEventPayload struct {
	CalendarID   string             `json:"calendar_id"`
	Title        string             `json:"title"`
	Description  string             `json:"description"`
	Location     string             `json:"location"`
	Start        calendar.EventTime `json:"start"`
	End          calendar.EventTime `json:"end"`
	AllDay       bool               `json:"all_day"`
	ExpectedETag string             `json:"expected_etag"`
}

func (payload createEventPayload) validate() error {
	if strings.TrimSpace(payload.Title) == "" {
		return errors.New("title is required")
	}
	if err := calendar.ValidateEventTimeRangeV2(payload.Start, payload.End); err != nil {
		return err
	}
	if payload.AllDay != payload.Start.IsAllDay() {
		return errors.New("all_day must match start and end")
	}
	return nil
}

type updateEventPayload struct {
	CalendarID   calendar.PatchField[string]             `json:"calendar_id"`
	Title        calendar.PatchField[string]             `json:"title"`
	Description  calendar.PatchField[string]             `json:"description"`
	Location     calendar.PatchField[string]             `json:"location"`
	Start        calendar.PatchField[calendar.EventTime] `json:"start"`
	End          calendar.PatchField[calendar.EventTime] `json:"end"`
	AllDay       calendar.PatchField[bool]               `json:"all_day"`
	ExpectedETag string                                  `json:"expected_etag"`
}

func decodeJSONPayload(w http.ResponseWriter, r *http.Request, payload any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(payload); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func decodeCreateEventPayload(w http.ResponseWriter, r *http.Request, payload *createEventPayload) error {
	if err := decodeJSONPayload(w, r, payload); err != nil {
		return err
	}
	return payload.validate()
}

func (s *Server) requireWritableCalendar(w http.ResponseWriter, r *http.Request, calendarID string) bool {
	items, err := s.store.ListAllCalendars(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "calendar data is unavailable")
		return false
	}
	for _, item := range items {
		if item.ID == calendarID && item.CanRead && item.CanWrite {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "calendar is not writable")
	return false
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	if s.app == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "native API is unavailable")
		return
	}
	var payload createEventPayload
	if err := decodeCreateEventPayload(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.requireWritableCalendar(w, r, payload.CalendarID) {
		return
	}
	event, err := s.app.CreateEvent(r.Context(), calendar.CreateEventRequestV2{
		CalendarID: payload.CalendarID,
		Event: calendar.EventCreateV2{
			Title: payload.Title, Description: payload.Description, Location: payload.Location,
			Start: payload.Start, End: payload.End,
		},
		Notifications: calendar.NotificationsNone,
	})
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, event)
}

func (s *Server) updateEvent(w http.ResponseWriter, r *http.Request) {
	if s.app == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "native API is unavailable")
		return
	}
	calendarID, eventID := r.PathValue("calendar_id"), r.PathValue("event_id")
	if calendarID == "" || eventID == "" {
		writeError(w, http.StatusBadRequest, "calendar_id and event_id are required")
		return
	}
	var payload updateEventPayload
	if err := decodeJSONPayload(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if payload.CalendarID.Present && (payload.CalendarID.Null || payload.CalendarID.Value != calendarID) {
		writeError(w, http.StatusBadRequest, "calendar_id cannot be changed")
		return
	}
	if !hasEventPatch(payload) {
		writeError(w, http.StatusBadRequest, "at least one editable event field is required")
		return
	}
	if payload.Title.Present && (payload.Title.Null || strings.TrimSpace(payload.Title.Value) == "") {
		writeError(w, http.StatusBadRequest, "title cannot be empty")
		return
	}
	if !s.requireWritableCalendar(w, r, calendarID) {
		return
	}
	existing, err := s.app.GetEvent(r.Context(), calendar.EventRef{CalendarID: calendarID, EventID: eventID})
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	if existing.RecurringEventID != "" || len(existing.Recurrence) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "recurring events cannot be edited")
		return
	}
	if err := validateUpdateTimeRange(payload, *existing); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	scope, expectedETag := ordinaryUpdateScope(existing.Provider, payload.ExpectedETag)
	result, err := s.app.UpdateEvent(r.Context(), calendar.UpdateEventRequestV2{
		Ref: calendar.EventRef{CalendarID: calendarID, EventID: eventID},
		Patch: calendar.EventPatchV2{
			Title: payload.Title, Description: payload.Description, Location: payload.Location,
			Start: payload.Start, End: payload.End,
		},
		Scope: scope, ExpectedETag: expectedETag, Notifications: calendar.NotificationsNone,
	})
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func hasEventPatch(payload updateEventPayload) bool {
	return payload.Title.Present || payload.Description.Present || payload.Location.Present || payload.Start.Present || payload.End.Present
}

func validateUpdateTimeRange(payload updateEventPayload, existing calendar.EventV2) error {
	start, end := existing.Start, existing.End
	if payload.Start.Present {
		if payload.Start.Null {
			return errors.New("start cannot be null")
		}
		start = payload.Start.Value
	}
	if payload.End.Present {
		if payload.End.Null {
			return errors.New("end cannot be null")
		}
		end = payload.End.Value
	}
	if payload.Start.Present || payload.End.Present {
		if err := calendar.ValidateEventTimeRangeV2(start, end); err != nil {
			return err
		}
	}
	if payload.AllDay.Present && (payload.AllDay.Null || payload.AllDay.Value != start.IsAllDay() || payload.AllDay.Value != end.IsAllDay()) {
		return errors.New("all_day must match start and end")
	}
	return nil
}

func ordinaryUpdateScope(provider, expectedETag string) (calendar.MutationScope, string) {
	if strings.EqualFold(provider, "apple") {
		return calendar.ScopeSeries, ""
	}
	return calendar.ScopeSingle, expectedETag
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if s.app == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "native API is unavailable")
		return
	}
	items, err := s.calendars(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "calendar data is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calendars": items})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if s.app == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "native API is unavailable")
		return
	}
	start, err := time.Parse(time.RFC3339, r.URL.Query().Get("start"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "start must be RFC3339")
		return
	}
	end, err := time.Parse(time.RFC3339, r.URL.Query().Get("end"))
	if err != nil || !end.After(start) {
		writeError(w, http.StatusBadRequest, "end must be RFC3339 and after start")
		return
	}
	allowed, err := s.calendars(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "calendar data is unavailable")
		return
	}
	allowedIDs := make(map[string]struct{}, len(allowed))
	for _, item := range allowed {
		allowedIDs[item.ID] = struct{}{}
	}
	ids := unique(r.URL.Query()["calendar_id"])
	if len(ids) == 0 {
		for _, item := range allowed {
			ids = append(ids, item.ID)
		}
	}
	items := make([]calendar.EventV2, 0)
	sources := make([]calendar.SourceStatus, 0)
	complete := true
	for _, id := range ids {
		if _, ok := allowedIDs[id]; !ok {
			writeError(w, http.StatusForbidden, "calendar is unavailable")
			return
		}
		page, err := s.app.ListEvents(r.Context(), calendar.ListEventsRequestV2{CalendarID: id, Start: start, End: end, View: calendar.RecurrenceExpanded})
		if err != nil {
			complete = false
			sources = append(sources, calendar.SourceStatus{CalendarID: id, Complete: false})
			continue
		}
		items = append(items, page.Items...)
		sources = append(sources, page.Sources...)
		complete = complete && page.Complete
	}
	sortEventsByStart(items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "sources": sources, "complete": complete})
}

// cachedEvents reads only the locally synchronized event projection. It must
// not fall back to app.ListEvents: a display request cannot trigger provider
// network I/O or wait on a slow CalDAV read.
func (s *Server) cachedEvents(w http.ResponseWriter, r *http.Request) {
	if s.cachedEventsApp == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "cached events are unavailable")
		return
	}
	start, end, ok := parseNativeEventRange(w, r)
	if !ok {
		return
	}
	allowed, err := s.calendars(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "calendar data is unavailable")
		return
	}
	allowedIDs := make(map[string]struct{}, len(allowed))
	for _, item := range allowed {
		allowedIDs[item.ID] = struct{}{}
	}
	ids := unique(r.URL.Query()["calendar_id"])
	if len(ids) == 0 {
		for _, item := range allowed {
			ids = append(ids, item.ID)
		}
	}
	for _, id := range ids {
		if _, ok := allowedIDs[id]; !ok {
			writeError(w, http.StatusForbidden, "calendar is unavailable")
			return
		}
	}

	events, statuses, err := s.cachedEventsApp.ListCachedEvents(r.Context(), ids, start, end)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	sortEventsByStart(events)
	items := make([]nativeEvent, 0, len(events))
	for _, event := range events {
		items = append(items, nativeEvent{
			CalendarID: event.CalendarID,
			Title:      event.Title,
			Status:     event.Status,
			Start:      nativeEventTime{Date: event.Start.Date, DateTime: event.Start.DateTime},
			End:        nativeEventTime{Date: event.End.Date, DateTime: event.End.DateTime},
		})
	}
	response := cachedEventsResponse{
		Items:    items,
		Sources:  make([]cachedSource, 0, len(ids)),
		Complete: true,
	}
	statusByCalendar := make(map[string]storage.CachedSourceStatus, len(statuses))
	for _, status := range statuses {
		statusByCalendar[status.CalendarID] = status
	}
	for _, id := range ids {
		status, found := statusByCalendar[id]
		if !found {
			response.Sources = append(response.Sources, cachedSource{CalendarID: id, Status: "pending", Stale: true})
			response.Complete = false
			continue
		}
		response.Sources = append(response.Sources, cachedSource{CalendarID: id, Status: status.Status, Stale: status.Stale})
		if status.Status != "ready" || status.Stale || status.WindowStart.After(start) || status.WindowEnd.Before(end) {
			response.Complete = false
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func parseNativeEventRange(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	start, err := time.Parse(time.RFC3339, r.URL.Query().Get("start"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "start must be RFC3339")
		return time.Time{}, time.Time{}, false
	}
	end, err := time.Parse(time.RFC3339, r.URL.Query().Get("end"))
	if err != nil || !end.After(start) {
		writeError(w, http.StatusBadRequest, "end must be RFC3339 and after start")
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

func sortEventsByStart(items []calendar.EventV2) {
	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		leftStart, leftErr := left.Start.Instant()
		rightStart, rightErr := right.Start.Instant()
		if leftErr == nil && rightErr == nil && !leftStart.Equal(rightStart) {
			return leftStart.Before(rightStart)
		}
		if leftErr == nil && rightErr == nil && left.Start.IsAllDay() != right.Start.IsAllDay() {
			return left.Start.IsAllDay()
		}
		if leftErr == nil && rightErr != nil {
			return true
		}
		if leftErr != nil && rightErr == nil {
			return false
		}
		leftValue, rightValue := left.Start.Date+left.Start.DateTime, right.Start.Date+right.Start.DateTime
		if leftValue != rightValue {
			return leftValue < rightValue
		}
		if left.CalendarID != right.CalendarID {
			return left.CalendarID < right.CalendarID
		}
		return left.ID < right.ID
	})
}

func unique(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			if _, ok := seen[value]; !ok {
				seen[value] = struct{}{}
				result = append(result, value)
			}
		}
	}
	return result
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeApplicationError(w http.ResponseWriter, err error) {
	var apiError *calendar.APIError
	if !errors.As(err, &apiError) {
		writeError(w, http.StatusInternalServerError, "calendar operation failed")
		return
	}
	status := http.StatusBadRequest
	switch apiError.Code {
	case calendar.ErrorConflict:
		status = http.StatusConflict
	case calendar.ErrorPermissionDenied:
		status = http.StatusForbidden
	case calendar.ErrorNotFound:
		status = http.StatusNotFound
	case calendar.ErrorUnsupportedCapability:
		status = http.StatusUnprocessableEntity
	case calendar.ErrorRateLimited:
		status = http.StatusTooManyRequests
	case calendar.ErrorProviderUnavailable, calendar.ErrorPartialFailure:
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"error": apiError.Message, "code": apiError.Code})
}
