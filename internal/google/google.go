package google

import (
	"context"
	"time"

	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/token"
)

type Provider struct {
	svc *gcal.Service
}

func New(clientID, clientSecret, refreshToken, tokenDir string) (*Provider, error) {
	store := token.NewFileStore(tokenDir, "google")
	cfg := newOAuthConfig(clientID, clientSecret)
	client := newHTTPClient(store, cfg, refreshToken)

	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}
	return &Provider{svc: svc}, nil
}

func (p *Provider) Name() string { return "google" }

func (p *Provider) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	list, err := p.svc.CalendarList.List().Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var cals []calendar.Calendar
	for _, c := range list.Items {
		cals = append(cals, calendar.Calendar{
			ID:       c.Id,
			Name:     c.Summary,
			Color:    c.BackgroundColor,
			Primary:  c.Primary,
			ReadOnly: c.AccessRole == "reader" || c.AccessRole == "freeBusyReader",
		})
	}
	return cals, nil
}

func (p *Provider) GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]calendar.Event, error) {
	events, err := p.svc.Events.List(calendarID).
		TimeMin(start.Format(time.RFC3339)).
		TimeMax(end.Format(time.RFC3339)).
		SingleEvents(true).
		OrderBy("startTime").
		Context(ctx).
		Do()
	if err != nil {
		return nil, err
	}
	var result []calendar.Event
	for _, e := range events.Items {
		result = append(result, convertEvent(e, calendarID))
	}
	return result, nil
}

func (p *Provider) CreateEvent(ctx context.Context, calendarID string, event calendar.EventCreate) (*calendar.Event, error) {
	ge := &gcal.Event{
		Summary:     event.Title,
		Description: event.Description,
		Location:    event.Location,
		Start:       &gcal.EventDateTime{DateTime: event.Start.Format(time.RFC3339)},
		End:         &gcal.EventDateTime{DateTime: event.End.Format(time.RFC3339)},
	}
	created, err := p.svc.Events.Insert(calendarID, ge).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	ev := convertEvent(created, calendarID)
	return &ev, nil
}

func (p *Provider) UpdateEvent(ctx context.Context, calendarID, eventID string, event calendar.EventUpdate) (*calendar.Event, error) {
	existing, err := p.svc.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	if event.Title != nil {
		existing.Summary = *event.Title
	}
	if event.Description != nil {
		existing.Description = *event.Description
	}
	if event.Location != nil {
		existing.Location = *event.Location
	}
	if event.Start != nil {
		existing.Start = &gcal.EventDateTime{DateTime: event.Start.Format(time.RFC3339)}
	}
	if event.End != nil {
		existing.End = &gcal.EventDateTime{DateTime: event.End.Format(time.RFC3339)}
	}
	updated, err := p.svc.Events.Update(calendarID, eventID, existing).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	ev := convertEvent(updated, calendarID)
	return &ev, nil
}

func (p *Provider) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	return p.svc.Events.Delete(calendarID, eventID).Context(ctx).Do()
}

func convertEvent(e *gcal.Event, calendarID string) calendar.Event {
	ev := calendar.Event{
		ID:          e.Id,
		CalendarID:  calendarID,
		Title:       e.Summary,
		Description: e.Description,
		Location:    e.Location,
		Status:      e.Status,
	}
	if e.Start != nil {
		if e.Start.DateTime != "" {
			ev.Start, _ = time.Parse(time.RFC3339, e.Start.DateTime)
		} else if e.Start.Date != "" {
			ev.Start, _ = time.Parse("2006-01-02", e.Start.Date)
			ev.AllDay = true
		}
	}
	if e.End != nil {
		if e.End.DateTime != "" {
			ev.End, _ = time.Parse(time.RFC3339, e.End.DateTime)
		} else if e.End.Date != "" {
			ev.End, _ = time.Parse("2006-01-02", e.End.Date)
		}
	}
	return ev
}
