package apple

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

type Provider struct {
	client     *caldav.Client
	username   string
	httpClient *http.Client
	caldavURL  string
}

func New(username, appPassword, caldavURL string) (*Provider, error) {
	httpClient := newBasicAuthClient(username, appPassword)
	client, err := caldav.NewClient(httpClient, caldavURL)
	if err != nil {
		return nil, fmt.Errorf("caldav client: %w", err)
	}
	return &Provider{
		client:     client,
		username:   username,
		httpClient: httpClient,
		caldavURL:  strings.TrimSuffix(caldavURL, "/"),
	}, nil
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
		// Apple returns HTTP 500 with malformed XML for Family Sharing and
		// delegated calendars where REPORT is broken server-side. Fall back to
		// PROPFIND + MultiGet which Apple handles correctly for these calendars.
		if strings.Contains(err.Error(), "XML syntax error") || strings.Contains(err.Error(), "unexpected EOF") {
			log.Printf("apple: calendar %s REPORT failed, trying PROPFIND+MultiGet fallback: %v", calendarID, err)
			return p.getEventsFallback(ctx, calendarID, start, end)
		}
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

// getEventsFallback retrieves events via PROPFIND (list .ics paths) + MultiGet
// for calendars where Apple's CalDAV REPORT is broken (e.g. Family Sharing).
func (p *Provider) getEventsFallback(ctx context.Context, calendarID string, start, end time.Time) ([]calendar.Event, error) {
	paths, err := p.propfindCalendarObjects(ctx, calendarID)
	if err != nil {
		log.Printf("apple: PROPFIND fallback failed for %s: %v", calendarID, err)
		return nil, nil
	}
	if len(paths) == 0 {
		return nil, nil
	}

	objects, err := p.client.MultiGetCalendar(ctx, calendarID, &caldav.CalendarMultiGet{
		Paths: paths,
	})
	if err != nil {
		log.Printf("apple: MultiGet fallback failed for %s: %v", calendarID, err)
		return nil, nil
	}

	var events []calendar.Event
	for _, obj := range objects {
		for _, ev := range obj.Data.Events() {
			dtStart, errS := ev.DateTimeStart(nil)
			dtEnd, errE := ev.DateTimeEnd(nil)
			if errS != nil || errE != nil {
				continue
			}
			if dtEnd.After(start) && dtStart.Before(end) {
				events = append(events, convertEvent(ev, calendarID, obj.Path))
			}
		}
	}
	return events, nil
}

// propfindCalendarObjects does a PROPFIND Depth:1 and returns paths of all
// .ics objects in the given calendar collection.
func (p *Provider) propfindCalendarObjects(ctx context.Context, calendarPath string) ([]string, error) {
	url := p.caldavURL + calendarPath
	body := []byte(`<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:propfind>`)

	req, err := http.NewRequestWithContext(ctx, "PROPFIND", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("PROPFIND returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var ms struct {
		XMLName   xml.Name `xml:"DAV: multistatus"`
		Responses []struct {
			Href string `xml:"DAV: href"`
		} `xml:"DAV: response"`
	}
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("PROPFIND parse: %w", err)
	}

	var paths []string
	for _, r := range ms.Responses {
		if strings.HasSuffix(r.Href, ".ics") {
			paths = append(paths, r.Href)
		}
	}
	return paths, nil
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
