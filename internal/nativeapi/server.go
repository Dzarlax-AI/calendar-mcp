// Package nativeapi exposes the narrow, token-authenticated read API used by
// calendar-app. It intentionally has no provider setup or event mutation routes.
package nativeapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type Config struct {
	App   *application.Service
	Store *storage.Store
	Token string
}

type Server struct {
	app   *application.Service
	store *storage.Store
	token string
}

func New(cfg Config) *Server { return &Server{app: cfg.App, store: cfg.Store, token: cfg.Token} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /bootstrap", s.authorized(http.HandlerFunc(s.bootstrap)))
	mux.Handle("GET /events", s.authorized(http.HandlerFunc(s.events)))
	return mux
}

func (s *Server) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.token == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
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
		result = append(result, calendarResponse{ID: item.ID, Name: item.Name, TimeZone: item.Timezone, CanRead: true, ReadOnly: !item.CanWrite})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
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
