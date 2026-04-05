package restapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

type Server struct {
	reg    *calendar.Registry
	apiKey string
}

func New(reg *calendar.Registry, apiKey string) *Server {
	return &Server{reg: reg, apiKey: apiKey}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/calendars", s.listCalendars)
	mux.HandleFunc("GET /api/events", s.getEvents)
	mux.HandleFunc("POST /api/events", s.createEvent)
	mux.HandleFunc("DELETE /api/events", s.deleteEvent)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return s.withAuth(mux)
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.Header.Get("X-API-Key")
		}
		if key != s.apiKey {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GET /api/calendars
func (s *Server) listCalendars(w http.ResponseWriter, r *http.Request) {
	cals, err := s.reg.ListCalendars(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cals)
}

// GET /api/events?calendar_id=google:primary&start=2026-04-05T00:00:00Z&end=2026-04-06T00:00:00Z
func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	calID := r.URL.Query().Get("calendar_id")
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")

	if startStr == "" || endStr == "" {
		writeError(w, http.StatusBadRequest, "start and end are required (ISO8601)")
		return
	}

	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid start: "+err.Error())
		return
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid end: "+err.Error())
		return
	}

	events, err := s.reg.GetEvents(r.Context(), calID, start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// POST /api/events
// Body: { "calendar_id": "...", "title": "...", "start": "...", "end": "...", "description": "...", "location": "...", "attendees": [...] }
func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CalendarID  string             `json:"calendar_id"`
		Title       string             `json:"title"`
		Start       string             `json:"start"`
		End         string             `json:"end"`
		Description string             `json:"description"`
		Location    string             `json:"location"`
		Attendees   []calendar.Attendee `json:"attendees"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.CalendarID == "" || req.Title == "" || req.Start == "" || req.End == "" {
		writeError(w, http.StatusBadRequest, "calendar_id, title, start, end are required")
		return
	}

	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid start: "+err.Error())
		return
	}
	end, err := time.Parse(time.RFC3339, req.End)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid end: "+err.Error())
		return
	}

	ev, err := s.reg.CreateEvent(r.Context(), req.CalendarID, calendar.EventCreate{
		Title:       req.Title,
		Start:       start,
		End:         end,
		Description: req.Description,
		Location:    req.Location,
		Attendees:   req.Attendees,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

// DELETE /api/events?calendar_id=google:primary&event_id=abc123
func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	calID := r.URL.Query().Get("calendar_id")
	eventID := r.URL.Query().Get("event_id")

	if calID == "" || eventID == "" {
		writeError(w, http.StatusBadRequest, "calendar_id and event_id are required")
		return
	}

	if err := s.reg.DeleteEvent(r.Context(), calID, eventID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
