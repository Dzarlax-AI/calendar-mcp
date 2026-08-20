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

	"golang.org/x/oauth2"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/token"
)

const graphBase = "https://graph.microsoft.com/v1.0"

type Provider struct {
	client    *http.Client
	baseURL   string
	routeName string
	calendars map[string]struct{}
}

func New(clientID, clientSecret, tenantID, refreshToken, tokenDir string) (*Provider, error) {
	store := token.NewFileStore(tokenDir, "microsoft")
	return NewWithTokenStore(clientID, clientSecret, tenantID, store, &oauth2.Token{RefreshToken: refreshToken})
}

func NewWithTokenStore(clientID, clientSecret, tenantID string, store token.Store, initial *oauth2.Token) (*Provider, error) {
	cfg := newOAuthConfig(clientID, clientSecret, tenantID)
	client := newHTTPClient(store, cfg, initial)
	return &Provider{client: client, baseURL: graphBase}, nil
}

func (p *Provider) Name() string { return "microsoft" }
func (p *Provider) RouteName() string {
	if p.routeName != "" {
		return p.routeName
	}
	return p.Name()
}
func (p *Provider) OwnsCalendar(id string) bool {
	if len(p.calendars) == 0 {
		return true
	}
	_, ok := p.calendars[id]
	return ok
}
func (p *Provider) SetRoute(route string, calendarIDs []string) {
	p.routeName = route
	p.calendars = make(map[string]struct{}, len(calendarIDs))
	for _, id := range calendarIDs {
		p.calendars[id] = struct{}{}
	}
}

func (p *Provider) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	type calendarPage struct {
		Value []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Color string `json:"color"`
			Owner struct {
				Name string `json:"name"`
			} `json:"owner"`
			CanEdit           bool `json:"canEdit"`
			IsDefaultCalendar bool `json:"isDefaultCalendar"`
		} `json:"value"`
		NextLink string `json:"@odata.nextLink"`
	}
	var cals []calendar.Calendar
	next := p.baseURL + "/me/calendars"
	seen := make(map[string]struct{})
	for next != "" {
		if _, ok := seen[next]; ok {
			return nil, fmt.Errorf("Graph pagination repeated next link")
		}
		seen[next] = struct{}{}
		var resp calendarPage
		if err := p.getURLWithHeaders(ctx, next, nil, &resp); err != nil {
			return nil, err
		}
		for _, c := range resp.Value {
			cals = append(cals, calendar.Calendar{
				ID:       c.ID,
				Name:     c.Name,
				Color:    c.Color,
				Primary:  c.IsDefaultCalendar,
				ReadOnly: !c.CanEdit,
			})
		}
		next = resp.NextLink
	}
	return cals, nil
}

func (p *Provider) GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]calendar.Event, error) {
	path := fmt.Sprintf("/me/calendars/%s/calendarView", url.PathEscape(calendarID))
	params := url.Values{
		"startDateTime": {start.UTC().Format("2006-01-02T15:04:05Z")},
		"endDateTime":   {end.UTC().Format("2006-01-02T15:04:05Z")},
		"$orderby":      {"start/dateTime"},
		"$top":          {"250"},
		"$select":       {"id,subject,body,start,end,location,isAllDay,showAs,attendees,onlineMeeting"},
	}

	type eventPage struct {
		Value    []graphEvent `json:"value"`
		NextLink string       `json:"@odata.nextLink"`
	}
	headers := http.Header{
		"Prefer": {`outlook.timezone="UTC"`, `outlook.body-content-type="text"`},
	}
	var events []calendar.Event
	next := p.baseURL + path + "?" + params.Encode()
	seen := make(map[string]struct{})
	for next != "" {
		if _, ok := seen[next]; ok {
			return nil, fmt.Errorf("Graph pagination repeated next link")
		}
		seen[next] = struct{}{}
		var resp eventPage
		if err := p.getURLWithHeaders(ctx, next, headers, &resp); err != nil {
			return nil, err
		}
		for _, e := range resp.Value {
			events = append(events, e.toEvent(calendarID))
		}
		next = resp.NextLink
	}
	return events, nil
}

func (p *Provider) CreateEvent(ctx context.Context, calendarID string, event calendar.EventCreate) (*calendar.Event, error) {
	if len(event.Attendees) > 0 {
		return nil, fmt.Errorf("microsoft attendee writes require an explicit notification policy")
	}
	path := fmt.Sprintf("/me/calendars/%s/events", url.PathEscape(calendarID))
	start := toGraphDateTime(event.Start)
	end := toGraphDateTime(event.End)
	body := graphEventCreate{
		Subject:  event.Title,
		IsAllDay: event.AllDay,
		Body: &graphBody{
			ContentType: "text",
			Content:     event.Description,
		},
		Start: start,
		End:   end,
	}
	if event.Location != "" {
		body.Location = &graphLocation{DisplayName: event.Location}
	}
	body.Attendees = toGraphAttendees(event.Attendees)
	if event.VideoCall {
		body.IsOnlineMeeting = true
		body.OnlineMeetingProvider = "teamsForBusiness"
	}

	var created graphEvent
	if err := p.post(ctx, path, body, &created); err != nil {
		return nil, err
	}
	ev := created.toEvent(calendarID)
	return &ev, nil
}

func (p *Provider) UpdateEvent(ctx context.Context, calendarID, eventID string, event calendar.EventUpdate) (*calendar.Event, error) {
	if event.Attendees != nil {
		return nil, fmt.Errorf("microsoft attendee writes require an explicit notification policy")
	}
	path := fmt.Sprintf("/me/calendars/%s/events/%s", url.PathEscape(calendarID), url.PathEscape(eventID))
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
		patch["start"] = toGraphDateTime(*event.Start)
	}
	if event.End != nil {
		patch["end"] = toGraphDateTime(*event.End)
	}
	if event.AllDay != nil {
		patch["isAllDay"] = *event.AllDay
	}
	var updated graphEvent
	if err := p.patch(ctx, path, patch, &updated); err != nil {
		return nil, err
	}
	ev := updated.toEvent(calendarID)
	return &ev, nil
}

func (p *Provider) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	path := fmt.Sprintf("/me/calendars/%s/events/%s", url.PathEscape(calendarID), url.PathEscape(eventID))
	return p.delete(ctx, path)
}

// HTTP helpers

func (p *Provider) get(ctx context.Context, path string, out any) error {
	return p.getWithParams(ctx, path, nil, out)
}

func (p *Provider) getWithParams(ctx context.Context, path string, params url.Values, out any) error {
	return p.getWithParamsAndHeaders(ctx, path, params, nil, out)
}

func (p *Provider) getWithParamsAndHeaders(ctx context.Context, path string, params url.Values, headers http.Header, out any) error {
	u := p.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	return p.getURLWithHeaders(ctx, u, headers, out)
}

func (p *Provider) getURLWithHeaders(ctx context.Context, rawURL string, headers http.Header, out any) error {
	u, err := p.validatedURL(rawURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return p.do(req, out)
}

func (p *Provider) post(ctx context.Context, path string, body, out any) error {
	return p.doJSON(ctx, "POST", path, body, out)
}

func (p *Provider) patch(ctx context.Context, path string, body, out any) error {
	return p.doJSON(ctx, "PATCH", path, body, out)
}

func (p *Provider) delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return graphAPIError(resp.StatusCode, body)
	}
	return nil
}

func (p *Provider) doJSON(ctx context.Context, method, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.do(req, out)
}

func (p *Provider) validatedURL(rawURL string) (string, error) {
	base, err := url.Parse(p.baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid Graph base URL: %w", err)
	}
	next, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid Graph URL: %w", err)
	}
	if !next.IsAbs() {
		next = base.ResolveReference(next)
	}
	if next.Scheme != base.Scheme || next.Host != base.Host {
		return "", fmt.Errorf("Graph next link points to unexpected origin: %s", next.Host)
	}
	return next.String(), nil
}

func (p *Provider) do(req *http.Request, out any) error {
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return graphAPIError(resp.StatusCode, body)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func graphAPIError(status int, body []byte) error {
	code := calendar.ErrorProviderUnavailable
	retryable := status >= 500
	switch status {
	case http.StatusBadRequest:
		code = calendar.ErrorInvalidArgument
	case http.StatusUnauthorized, http.StatusForbidden:
		code = calendar.ErrorPermissionDenied
	case http.StatusNotFound, http.StatusGone:
		code = calendar.ErrorNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		code = calendar.ErrorConflict
	case http.StatusTooManyRequests:
		code, retryable = calendar.ErrorRateLimited, true
	}
	return &calendar.APIError{Code: code, Message: fmt.Sprintf("Graph API %d: %s", status, body), Provider: "microsoft", Retryable: retryable}
}

// Graph API types

type graphEvent struct {
	ID      string `json:"id"`
	ETag    string `json:"@odata.etag"`
	ICalUID string `json:"iCalUId"`
	Subject string `json:"subject"`
	Body    struct {
		Content string `json:"content"`
	} `json:"body"`
	Start                graphDateTime    `json:"start"`
	End                  graphDateTime    `json:"end"`
	Location             graphLocation    `json:"location"`
	IsAllDay             bool             `json:"isAllDay"`
	IsCancelled          bool             `json:"isCancelled"`
	ShowAs               string           `json:"showAs"`
	Sensitivity          string           `json:"sensitivity"`
	SeriesMasterID       string           `json:"seriesMasterId"`
	Type                 string           `json:"type"`
	OriginalStart        string           `json:"originalStart"`
	OccurrenceID         string           `json:"occurrenceId"`
	Recurrence           *graphRecurrence `json:"recurrence"`
	CancelledOccurrences []string         `json:"cancelledOccurrences"`
	Attendees            []graphAttendee  `json:"attendees,omitempty"`
	OnlineMeeting        *struct {
		JoinUrl string `json:"joinUrl"`
	} `json:"onlineMeeting,omitempty"`
	SingleValueExtendedProperties []graphSingleValueProperty `json:"singleValueExtendedProperties,omitempty"`
}

type graphSingleValueProperty struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type graphRecurrence struct {
	Pattern graphRecurrencePattern `json:"pattern"`
	Range   graphRecurrenceRange   `json:"range"`
}
type graphRecurrencePattern struct {
	Type           string   `json:"type"`
	Interval       int      `json:"interval"`
	Month          int      `json:"month,omitempty"`
	DayOfMonth     int      `json:"dayOfMonth,omitempty"`
	DaysOfWeek     []string `json:"daysOfWeek,omitempty"`
	FirstDayOfWeek string   `json:"firstDayOfWeek,omitempty"`
	Index          string   `json:"index,omitempty"`
}
type graphRecurrenceRange struct {
	Type                string `json:"type"`
	StartDate           string `json:"startDate"`
	EndDate             string `json:"endDate,omitempty"`
	NumberOfOccurrences int    `json:"numberOfOccurrences,omitempty"`
	RecurrenceTimeZone  string `json:"recurrenceTimeZone,omitempty"`
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

func toGraphDateTime(t time.Time) graphDateTime {
	return graphDateTime{
		DateTime: t.UTC().Format("2006-01-02T15:04:05"),
		TimeZone: "UTC",
	}
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphLocation struct {
	DisplayName string `json:"displayName"`
}

type graphEventCreate struct {
	Subject                       string                     `json:"subject"`
	Body                          *graphBody                 `json:"body,omitempty"`
	Start                         graphDateTime              `json:"start"`
	End                           graphDateTime              `json:"end"`
	Location                      *graphLocation             `json:"location,omitempty"`
	Attendees                     []graphAttendee            `json:"attendees,omitempty"`
	IsAllDay                      bool                       `json:"isAllDay,omitempty"`
	IsOnlineMeeting               bool                       `json:"isOnlineMeeting,omitempty"`
	OnlineMeetingProvider         string                     `json:"onlineMeetingProvider,omitempty"`
	Recurrence                    *graphRecurrence           `json:"recurrence,omitempty"`
	SingleValueExtendedProperties []graphSingleValueProperty `json:"singleValueExtendedProperties,omitempty"`
}

func toGraphAttendees(attendees []calendar.Attendee) []graphAttendee {
	if len(attendees) == 0 {
		return []graphAttendee{}
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

func parseGraphTime(dt graphDateTime) time.Time {
	loc := time.UTC
	if dt.TimeZone != "" && dt.TimeZone != "UTC" {
		if tz, err := time.LoadLocation(dt.TimeZone); err == nil {
			loc = tz
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.0000000", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, dt.DateTime, loc); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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
	ev.Start = parseGraphTime(e.Start)
	ev.End = parseGraphTime(e.End)
	if e.OnlineMeeting != nil && e.OnlineMeeting.JoinUrl != "" {
		ev.OnlineMeeting = e.OnlineMeeting.JoinUrl
	}
	return ev
}
