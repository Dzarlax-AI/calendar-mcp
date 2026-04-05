package apple

import (
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

type Provider struct {
	client   *caldav.Client
	username string
}

func New(username, appPassword, caldavURL string) (*Provider, error) {
	client, err := caldav.NewClient(
		newBasicAuthClient(username, appPassword),
		caldavURL,
	)
	if err != nil {
		return nil, fmt.Errorf("caldav client: %w", err)
	}
	return &Provider{client: client, username: username}, nil
}

func (p *Provider) Name() string { return "apple" }

func (p *Provider) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	principal, err := p.client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("find principal: %w", err)
	}
	homeSet, err := p.client.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("find home set: %w", err)
	}
	davCals, err := p.client.FindCalendars(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("find calendars: %w", err)
	}
	var cals []calendar.Calendar
	for _, c := range davCals {
		cals = append(cals, calendar.Calendar{
			ID:   c.Path,
			Name: c.Name,
		})
	}
	return cals, nil
}

func (p *Provider) GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]calendar.Event, error) {
	query := &caldav.CalendarQuery{
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{{
				Name:  "VEVENT",
				Start: start,
				End:   end,
			}},
		},
	}
	objects, err := p.client.QueryCalendar(ctx, calendarID, query)
	if err != nil {
		return nil, err
	}
	var events []calendar.Event
	for _, obj := range objects {
		for _, ev := range obj.Data.Events() {
			events = append(events, convertEvent(ev, calendarID, obj.Path))
		}
	}
	return events, nil
}

func (p *Provider) CreateEvent(ctx context.Context, calendarID string, event calendar.EventCreate) (*calendar.Event, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//calendar-mcp//EN")

	vevent := ical.NewEvent()
	uid := fmt.Sprintf("%d@calendar-mcp", time.Now().UnixNano())
	vevent.Props.SetText(ical.PropUID, uid)
	vevent.Props.SetText(ical.PropSummary, event.Title)
	vevent.Props.SetDateTime(ical.PropDateTimeStart, event.Start)
	vevent.Props.SetDateTime(ical.PropDateTimeEnd, event.End)
	if event.Description != "" {
		vevent.Props.SetText(ical.PropDescription, event.Description)
	}
	if event.Location != "" {
		vevent.Props.SetText(ical.PropLocation, event.Location)
	}
	addAttendees(vevent, event.Attendees)
	cal.Children = append(cal.Children, vevent.Component)

	path := calendarID + uid + ".ics"
	_, err := p.client.PutCalendarObject(ctx, path, cal)
	if err != nil {
		return nil, err
	}
	ev := calendar.Event{
		ID:          uid,
		CalendarID:  calendarID,
		Title:       event.Title,
		Description: event.Description,
		Location:    event.Location,
		Start:       event.Start,
		End:         event.End,
	}
	return &ev, nil
}

func (p *Provider) UpdateEvent(ctx context.Context, calendarID, eventID string, event calendar.EventUpdate) (*calendar.Event, error) {
	path := calendarID + eventID + ".ics"

	objects, err := p.client.MultiGetCalendar(ctx, calendarID, &caldav.CalendarMultiGet{
		Paths: []string{path},
	})
	if err != nil || len(objects) == 0 {
		return nil, fmt.Errorf("event not found: %s", eventID)
	}
	obj := objects[0]

	for _, vevent := range obj.Data.Events() {
		if event.Title != nil {
			vevent.Props.SetText(ical.PropSummary, *event.Title)
		}
		if event.Description != nil {
			vevent.Props.SetText(ical.PropDescription, *event.Description)
		}
		if event.Location != nil {
			vevent.Props.SetText(ical.PropLocation, *event.Location)
		}
		if event.Start != nil {
			vevent.Props.SetDateTime(ical.PropDateTimeStart, *event.Start)
		}
		if event.End != nil {
			vevent.Props.SetDateTime(ical.PropDateTimeEnd, *event.End)
		}
	}

	_, err = p.client.PutCalendarObject(ctx, path, obj.Data)
	if err != nil {
		return nil, err
	}

	existing := obj.Data.Events()[0]
	ev := convertEvent(existing, calendarID, path)
	return &ev, nil
}

func (p *Provider) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	path := calendarID + eventID + ".ics"
	return p.client.RemoveAll(ctx, path)
}

func addAttendees(vevent *ical.Event, attendees []calendar.Attendee) {
	for _, a := range attendees {
		prop := ical.NewProp("ATTENDEE")
		prop.Value = "mailto:" + a.Email
		if a.Name != "" {
			prop.Params.Set("CN", a.Name)
		}
		role := "REQ-PARTICIPANT"
		if a.Optional {
			role = "OPT-PARTICIPANT"
		}
		prop.Params.Set("ROLE", role)
		prop.Params.Set("RSVP", "TRUE")
		vevent.Props.Add(prop)
	}
}

func parseAttendees(ev ical.Event) []calendar.Attendee {
	var out []calendar.Attendee
	for _, prop := range ev.Props.Values("ATTENDEE") {
		email := prop.Value
		if len(email) > 7 && email[:7] == "mailto:" {
			email = email[7:]
		}
		name := ""
		if cn := prop.Params.Get("CN"); cn != "" {
			name = cn
		}
		status := ""
		if ps := prop.Params.Get("PARTSTAT"); ps != "" {
			status = ps
		}
		optional := false
		if role := prop.Params.Get("ROLE"); role == "OPT-PARTICIPANT" {
			optional = true
		}
		out = append(out, calendar.Attendee{
			Email:    email,
			Name:     name,
			Status:   status,
			Optional: optional,
		})
	}
	return out
}

func convertEvent(ev ical.Event, calendarID, path string) calendar.Event {
	uid, _ := ev.Props.Text(ical.PropUID)
	summary, _ := ev.Props.Text(ical.PropSummary)
	desc, _ := ev.Props.Text(ical.PropDescription)
	loc, _ := ev.Props.Text(ical.PropLocation)
	dtStart, _ := ev.DateTimeStart(nil)
	dtEnd, _ := ev.DateTimeEnd(nil)

	return calendar.Event{
		ID:          uid,
		CalendarID:  calendarID,
		Title:       summary,
		Description: desc,
		Location:    loc,
		Start:       dtStart,
		End:         dtEnd,
		Attendees:   parseAttendees(ev),
	}
}
