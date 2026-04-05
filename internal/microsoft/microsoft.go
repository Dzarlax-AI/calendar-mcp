package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/token"
)

const graphBase = "https://graph.microsoft.com/v1.0"

type Provider struct {
	client *http.Client
}

func New(clientID, clientSecret, tenantID, refreshToken, tokenDir string) (*Provider, error) {
	store := token.NewFileStore(tokenDir, "microsoft")
	cfg := newOAuthConfig(clientID, clientSecret, tenantID)
	client := newHTTPClient(store, cfg, refreshToken)
	return &Provider{client: client}, nil
}

func (p *Provider) Name() string { return "microsoft" }

func (p *Provider) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	var resp struct {
		Value []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Color string `json:"color"`
			Owner struct {
				Name string `json:"name"`
			} `json:"owner"`
			CanEdit          bool `json:"canEdit"`
			IsDefaultCalendar bool `json:"isDefaultCalendar"`
		} `json:"value"`
	}
	if err := p.get(ctx, "/me/calendars", &resp); err != nil {
		return nil, err
	}
	var cals []calendar.Calendar
	for _, c := range resp.Value {
		cals = append(cals, calendar.Calendar{
			ID:       c.ID,
			Name:     c.Name,
			Color:    c.Color,
			Primary:  c.IsDefaultCalendar,
			ReadOnly: !c.CanEdit,
		})
	}
	return cals, nil
}

func (p *Provider) GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]calendar.Event, error) {
	path := fmt.Sprintf("/me/calendars/%s/events", calendarID)
	params := url.Values{
		"$filter":  {fmt.Sprintf("start/dateTime ge '%s' and end/dateTime le '%s'", start.Format(time.RFC3339), end.Format(time.RFC3339))},
		"$orderby": {"start/dateTime"},
		"$top":     {"250"},
	}

	var resp struct {
		Value []graphEvent `json:"value"`
	}
	if err := p.getWithParams(ctx, path, params, &resp); err != nil {
		return nil, err
	}
	var events []calendar.Event
	for _, e := range resp.Value {
		events = append(events, e.toEvent(calendarID))
	}
	return events, nil
}

func (p *Provider) CreateEvent(ctx context.Context, calendarID string, event calendar.EventCreate) (*calendar.Event, error) {
	path := fmt.Sprintf("/me/calendars/%s/events", calendarID)
	body := graphEventCreate{
		Subject: event.Title,
		Body: &graphBody{
			ContentType: "text",
			Content:     event.Description,
		},
		Start: graphDateTime{
			DateTime: event.Start.Format("2006-01-02T15:04:05"),
			TimeZone: "UTC",
		},
		End: graphDateTime{
			DateTime: event.End.Format("2006-01-02T15:04:05"),
			TimeZone: "UTC",
		},
	}
	if event.Location != "" {
		body.Location = &graphLocation{DisplayName: event.Location}
	}
	body.Attendees = toGraphAttendees(event.Attendees)

	var created graphEvent
	if err := p.post(ctx, path, body, &created); err != nil {
		return nil, err
	}
	ev := created.toEvent(calendarID)
	return &ev, nil
}

func (p *Provider) UpdateEvent(ctx context.Context, calendarID, eventID string, event calendar.EventUpdate) (*calendar.Event, error) {
	path := fmt.Sprintf("/me/calendars/%s/events/%s", calendarID, eventID)
	patch := make(map[string]any)
	if event.Title != nil {
		patch["subject"] = *event.Title
	}
	if event.Description != nil {
		patch["body"] = graphBody{ContentType: "text", Content: *event.Description}
	}
	if event.Location != nil {
		patch["location"] = graphLocation{DisplayName: *event.Location}
	}
	if event.Start != nil {
		patch["start"] = graphDateTime{DateTime: event.Start.Format("2006-01-02T15:04:05"), TimeZone: "UTC"}
	}
	if event.End != nil {
		patch["end"] = graphDateTime{DateTime: event.End.Format("2006-01-02T15:04:05"), TimeZone: "UTC"}
	}
	if len(event.Attendees) > 0 {
		patch["attendees"] = toGraphAttendees(event.Attendees)
	}

	var updated graphEvent
	if err := p.patch(ctx, path, patch, &updated); err != nil {
		return nil, err
	}
	ev := updated.toEvent(calendarID)
	return &ev, nil
}

func (p *Provider) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	path := fmt.Sprintf("/me/calendars/%s/events/%s", calendarID, eventID)
	return p.delete(ctx, path)
}

// HTTP helpers

func (p *Provider) get(ctx context.Context, path string, out any) error {
	return p.getWithParams(ctx, path, nil, out)
}

func (p *Provider) getWithParams(ctx context.Context, path string, params url.Values, out any) error {
	u := graphBase + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	return p.do(req, out)
}

func (p *Provider) post(ctx context.Context, path string, body, out any) error {
	return p.doJSON(ctx, "POST", path, body, out)
}

func (p *Provider) patch(ctx context.Context, path string, body, out any) error {
	return p.doJSON(ctx, "PATCH", path, body, out)
}

func (p *Provider) delete(ctx context.Context, path string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", graphBase+path, nil)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("graph API %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (p *Provider) doJSON(ctx context.Context, method, path string, body, out any) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, method, graphBase+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	return p.do(req, out)
}

func (p *Provider) do(req *http.Request, out any) error {
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("graph API %d: %s", resp.StatusCode, body)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// Graph API types

type graphEvent struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Body    struct {
		Content string `json:"content"`
	} `json:"body"`
	Start       graphDateTime   `json:"start"`
	End         graphDateTime   `json:"end"`
	Location    graphLocation   `json:"location"`
	IsAllDay    bool            `json:"isAllDay"`
	ShowAs      string          `json:"showAs"`
	Attendees   []graphAttendee `json:"attendees,omitempty"`
}

type graphAttendeeStatus struct {
	Response string `json:"response"` // accepted, declined, tentativelyAccepted, none
}

type graphAttendee struct {
	EmailAddress struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	} `json:"emailAddress"`
	Type   string               `json:"type"`             // required, optional
	Status *graphAttendeeStatus `json:"status,omitempty"` // nil when creating
}

type graphDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphLocation struct {
	DisplayName string `json:"displayName"`
}

type graphEventCreate struct {
	Subject   string          `json:"subject"`
	Body      *graphBody      `json:"body,omitempty"`
	Start     graphDateTime   `json:"start"`
	End       graphDateTime   `json:"end"`
	Location  *graphLocation  `json:"location,omitempty"`
	Attendees []graphAttendee `json:"attendees,omitempty"`
}

func toGraphAttendees(attendees []calendar.Attendee) []graphAttendee {
	if len(attendees) == 0 {
		return nil
	}
	var out []graphAttendee
	for _, a := range attendees {
		typ := "required"
		if a.Optional {
			typ = "optional"
		}
		out = append(out, graphAttendee{
			EmailAddress: struct {
				Address string `json:"address"`
				Name    string `json:"name"`
			}{Address: a.Email, Name: a.Name},
			Type: typ,
		})
	}
	return out
}

func fromGraphAttendees(attendees []graphAttendee) []calendar.Attendee {
	if len(attendees) == 0 {
		return nil
	}
	var out []calendar.Attendee
	for _, a := range attendees {
		status := ""
		if a.Status != nil {
			status = a.Status.Response
		}
		out = append(out, calendar.Attendee{
			Email:    a.EmailAddress.Address,
			Name:     a.EmailAddress.Name,
			Status:   status,
			Optional: a.Type == "optional",
		})
	}
	return out
}

func (e *graphEvent) toEvent(calendarID string) calendar.Event {
	ev := calendar.Event{
		ID:          e.ID,
		CalendarID:  calendarID,
		Title:       e.Subject,
		Description: e.Body.Content,
		Location:    e.Location.DisplayName,
		AllDay:      e.IsAllDay,
		Status:      e.ShowAs,
		Attendees:   fromGraphAttendees(e.Attendees),
	}
	ev.Start, _ = time.Parse("2006-01-02T15:04:05.0000000", e.Start.DateTime)
	if ev.Start.IsZero() {
		ev.Start, _ = time.Parse("2006-01-02T15:04:05", e.Start.DateTime)
	}
	ev.End, _ = time.Parse("2006-01-02T15:04:05.0000000", e.End.DateTime)
	if ev.End.IsZero() {
		ev.End, _ = time.Parse("2006-01-02T15:04:05", e.End.DateTime)
	}
	return ev
}
