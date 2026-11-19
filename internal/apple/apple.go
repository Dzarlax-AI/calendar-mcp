package apple

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

type Provider struct {
	client             *caldav.Client
	username           string
	httpClient         *http.Client
	caldavURL          string
	routeName          string
	calendars          map[string]struct{}
	credentialRevision string
	credentialDigest   [32]byte
	discovery          discoveryCache
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

// SetCredentialBinding is called before the provider is published to a registry.
func (p *Provider) SetCredentialBinding(revision string, digest [32]byte) {
	p.credentialRevision, p.credentialDigest = revision, digest
}
func (p *Provider) CredentialBinding() (string, [32]byte) {
	return p.credentialRevision, p.credentialDigest
}

func (p *Provider) Name() string { return "apple" }
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
	inventory, err := p.discoverCalendars(ctx, false)
	return inventory.calendars, err
}

func (p *Provider) discoverCalendars(ctx context.Context, allowPartial bool) (calendarInventory, error) {
	homeSet, generation, err := p.calendarHomeSet(ctx)
	if err != nil {
		return calendarInventory{}, err
	}
	status := &discoveryStatus{}
	readCtx := context.WithValue(ctx, discoveryStatusKey{}, status)
	inventory, err := p.findCalendarInventory(readCtx, homeSet, allowPartial)
	code := status.code.Load()
	if code == 404 || code == 410 || code == 401 || (code == 403 && !inventory.timezoneOnlyDenied) {
		p.invalidateDiscovery(generation)
		if err == nil {
			err = fmt.Errorf("CalDAV discovery HTTP %d", code)
		}
	}
	if err != nil {
		return calendarInventory{}, fmt.Errorf("find calendars: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return calendarInventory{}, err
	}
	return inventory, nil
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
	setAppleEventTime(*vevent, ical.PropDateTimeStart, event.Start, event.AllDay)
	setAppleEventTime(*vevent, ical.PropDateTimeEnd, event.End, event.AllDay)
	if event.Description != "" {
		vevent.Props.SetText(ical.PropDescription, event.Description)
	}
	if event.Location != "" {
		vevent.Props.SetText(ical.PropLocation, event.Location)
	}
	addAttendees(vevent, event.Attendees)
	cal.Children = append(cal.Children, vevent.Component)

	path, err := appleObjectPath(calendarID, uid)
	if err != nil {
		return nil, calendar.DefinitiveNoDispatch(err)
	}
	_, err = p.client.PutCalendarObject(ctx, path, cal)
	if err != nil {
		return nil, err
	}
	ev := newCreatedEvent(calendarID, path, event)
	return &ev, nil
}

func (p *Provider) UpdateEvent(ctx context.Context, calendarID, eventID string, event calendar.EventUpdate) (*calendar.Event, error) {
	path, err := appleObjectPath(calendarID, eventID)
	if err != nil {
		return nil, calendar.DefinitiveNoDispatch(err)
	}

	objects, err := p.client.MultiGetCalendar(ctx, calendarID, &caldav.CalendarMultiGet{
		Paths: []string{path},
	})
	if err != nil || len(objects) == 0 {
		return nil, calendar.DefinitiveNoDispatch(fmt.Errorf("event not found: %s", eventID))
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
		allDay := appleEventAllDay(vevent)
		if event.AllDay != nil {
			allDay = *event.AllDay
		}
		if event.Start != nil {
			setAppleEventTime(vevent, ical.PropDateTimeStart, *event.Start, allDay)
		}
		if event.End != nil {
			setAppleEventTime(vevent, ical.PropDateTimeEnd, *event.End, allDay)
		}
		if event.Attendees != nil {
			vevent.Props.Del("ATTENDEE")
			addAttendees(&vevent, *event.Attendees)
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
	path, err := appleObjectPath(calendarID, eventID)
	if err != nil {
		return calendar.DefinitiveNoDispatch(err)
	}
	return p.client.RemoveAll(ctx, path)
}

func newCreatedEvent(calendarID, eventID string, event calendar.EventCreate) calendar.Event {
	return calendar.Event{
		ID:          eventID,
		CalendarID:  calendarID,
		Title:       event.Title,
		Description: event.Description,
		Location:    event.Location,
		Start:       event.Start,
		End:         event.End,
		AllDay:      event.AllDay,
	}
}

func appleObjectPath(calendarID, eventID string) (string, error) {
	base := pathpkg.Clean("/" + strings.TrimSpace(calendarID))
	if base == "/" {
		return "", fmt.Errorf("calendar ID is empty")
	}
	base += "/"
	if eventID == "" {
		return "", fmt.Errorf("event ID is empty")
	}

	var candidate string
	if strings.HasSuffix(strings.ToLower(eventID), ".ics") {
		if strings.Contains(eventID, "/") {
			candidate = pathpkg.Clean("/" + eventID)
		} else {
			candidate = pathpkg.Join(base, eventID)
		}
	} else {
		candidate = pathpkg.Join(base, url.PathEscape(eventID)+".ics")
	}
	if !strings.HasPrefix(candidate, base) {
		return "", fmt.Errorf("event path %q is outside calendar %q", eventID, calendarID)
	}
	return candidate, nil
}

func setAppleEventTime(vevent ical.Event, name string, t time.Time, allDay bool) {
	if allDay {
		vevent.Props.SetDate(name, t)
		return
	}
	vevent.Props.SetDateTime(name, t)
}

func appleEventAllDay(ev ical.Event) bool {
	prop := ev.Props.Get(ical.PropDateTimeStart)
	return prop != nil && prop.ValueType() == ical.ValueDate
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

const fallbackConcurrency = 20

// getEventsFallback retrieves events via PROPFIND (list .ics paths) + concurrent
// individual GETs for calendars where Apple's CalDAV REPORT is broken (Family Sharing).
func (p *Provider) getEventsFallback(ctx context.Context, calendarID string, start, end time.Time) ([]calendar.Event, error) {
	objects, err := p.getCalendarObjectsFallback(ctx, calendarID)
	if err != nil {
		return nil, err
	}
	var events []calendar.Event
	var parseErrs []error
	for _, obj := range objects {
		for _, ev := range obj.Data.Events() {
			dtStart, errS := ev.DateTimeStart(nil)
			dtEnd, errE := ev.DateTimeEnd(nil)
			if errS != nil || errE != nil {
				parseErrs = append(parseErrs, fmt.Errorf("parse event time from %s", obj.Path))
				continue
			}
			if dtEnd.After(start) && dtStart.Before(end) {
				events = append(events, convertEvent(ev, calendarID, obj.Path))
			}
		}
	}
	if len(parseErrs) > 0 {
		return nil, fmt.Errorf("apple GET fallback incomplete: %w", errors.Join(parseErrs...))
	}
	return events, nil
}

func (p *Provider) getCalendarObjectsFallback(ctx context.Context, calendarID string) ([]caldav.CalendarObject, error) {
	return p.getCalendarObjectsFallbackWithCompleteness(ctx, calendarID, false)
}

// Strict callers cannot treat a vanished object as a proven complete snapshot.
func (p *Provider) getCalendarObjectsFallbackWithCompleteness(ctx context.Context, calendarID string, strict bool) ([]caldav.CalendarObject, error) {
	collection, err := url.Parse(p.caldavURL)
	if err != nil {
		return nil, fmt.Errorf("invalid CalDAV fallback origin")
	}
	collection.Path = strings.TrimSuffix(calendarID, "/") + "/"
	collection.RawPath = ""
	client := *p.httpClient
	// The authentication transport sets Basic auth on every request. Only
	// send it to the configured origin, including after a redirect response.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	responses, err := readAppleFallbackInventory(ctx, &client, collection.String())
	if err != nil {
		return nil, err
	}
	objectsByPath := make(map[string]string, len(responses))
	var endpoints []*url.URL
	for _, item := range responses {
		href := strings.TrimSpace(item.Href)
		pathHint := href
		if end := strings.IndexAny(href, "?#"); end >= 0 {
			pathHint = href[:end]
		}
		decoded, err := url.PathUnescape(pathHint)
		if err != nil {
			return nil, fmt.Errorf("Apple fallback returned malformed href")
		}
		if !strings.HasSuffix(strings.ToLower(decoded), ".ics") {
			continue
		}

		endpoint, err := appleFallbackObjectURL(collection, href)
		if err != nil {
			return nil, err
		}
		if strict {
			if err := checkAppleFallbackStatus(item); err != nil {
				return nil, err
			}
		}
		if existing, seen := objectsByPath[endpoint.Path]; seen {
			if existing != endpoint.EscapedPath() && strict {
				return nil, fmt.Errorf("Apple fallback has conflicting object hrefs")
			}
			continue
		}
		objectsByPath[endpoint.Path] = endpoint.EscapedPath()
		endpoints = append(endpoints, endpoint)
	}

	type slot struct {
		object  *caldav.CalendarObject
		err     error
		missing bool
	}
	results := make([]slot, len(endpoints))
	sem := make(chan struct{}, fallbackConcurrency)
	var wg sync.WaitGroup

	for i, endpoint := range endpoints {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			object, missing, err := readAppleFallbackObject(ctx, &client, endpoint)
			results[i] = slot{object: object, missing: missing, err: err}
		}()
	}

	wg.Wait()

	var objects []caldav.CalendarObject
	var objectErrs []error
	missingCount := 0
	for _, r := range results {
		if r.object != nil {
			objects = append(objects, *r.object)
		}
		if r.missing {
			missingCount++
		}
		if r.err != nil {
			objectErrs = append(objectErrs, r.err)
		}
	}
	if missingCount > 0 {
		if strict {
			return nil, fmt.Errorf("apple GET fallback incomplete: %d object(s) disappeared after PROPFIND", missingCount)
		}
		log.Printf("apple: GET fallback for %s skipped %d object(s) deleted after PROPFIND", calendarID, missingCount)
	}
	if len(objectErrs) > 0 {
		return nil, fmt.Errorf("apple GET fallback incomplete: %w", errors.Join(objectErrs...))
	}
	return objects, nil
}

func appleFallbackObjectURL(collection *url.URL, href string) (*url.URL, error) {
	ref, err := url.Parse(href)
	if err != nil || ref.Opaque != "" || ref.User != nil || ref.ForceQuery || ref.RawQuery != "" || strings.Contains(href, "#") ||
		(ref.Scheme != "" && (ref.Scheme != collection.Scheme || !strings.EqualFold(ref.Host, collection.Host))) ||
		(ref.Host != "" && !strings.EqualFold(ref.Host, collection.Host)) {
		return nil, fmt.Errorf("Apple fallback returned unsafe object href")
	}
	escaped := strings.ToLower(ref.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(ref.Path, "\\") ||
		strings.Contains("/"+ref.Path+"/", "/../") || strings.Contains("/"+ref.Path+"/", "/./") {
		return nil, fmt.Errorf("Apple fallback returned unsafe object path")
	}
	endpoint := collection.ResolveReference(ref)
	if pathpkg.Dir(endpoint.Path) != strings.TrimSuffix(collection.Path, "/") ||
		!strings.HasSuffix(strings.ToLower(endpoint.Path), ".ics") {
		return nil, fmt.Errorf("Apple fallback object is outside the collection")
	}
	return endpoint, nil
}

func checkAppleFallbackStatus(item appleSyncResponse) error {
	if item.Status != "" && webDAVStatus(item.Status)/100 != 2 {
		return fmt.Errorf("Apple fallback returned an unsuccessful object status")
	}
	for _, prop := range item.PropStats {
		switch webDAVStatus(prop.Status) {
		case http.StatusOK, http.StatusNotFound:
			// A 404 propstat says getetag was absent. The object's
			// existence still needs to be checked with GET.
		default:
			return fmt.Errorf("Apple fallback returned an unsuccessful property status")
		}
	}
	return nil
}

func readAppleFallbackObject(ctx context.Context, client *http.Client, endpoint *url.URL) (*caldav.CalendarObject, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("invalid CalDAV object request")
	}
	req.Header.Set("Accept", ical.MIMEType)
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return nil, true, nil
	case http.StatusOK:
	default:
		return nil, false, fmt.Errorf("CalDAV fallback GET returned HTTP %d", resp.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, ical.MIMEType) {
		return nil, false, fmt.Errorf("CalDAV fallback GET returned an invalid content type")
	}
	data, err := ical.NewDecoder(resp.Body).Decode()
	if err != nil {
		return nil, false, err
	}
	object := &caldav.CalendarObject{Path: endpoint.Path, Data: data}
	if raw := resp.Header.Get("ETag"); raw != "" {
		object.ETag, err = strconv.Unquote(raw)
		if err != nil {
			return nil, false, fmt.Errorf("CalDAV fallback GET returned an invalid ETag")
		}
	}
	return object, false, nil
}

// propfindCalendarObjects is used by ordinary event sync. It preserves the
// raw href spelling used by the existing sync inventory.
func (p *Provider) propfindCalendarObjects(ctx context.Context, calendarPath string) ([]string, error) {
	responses, err := readAppleFallbackInventory(ctx, p.httpClient, p.caldavURL+calendarPath)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, item := range responses {
		if strings.HasSuffix(item.Href, ".ics") {
			paths = append(paths, item.Href)
		}
	}
	return paths, nil
}

func readAppleFallbackInventory(ctx context.Context, client *http.Client, endpoint string) ([]appleSyncResponse, error) {
	body := []byte(`<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:propfind>`)
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("PROPFIND returned HTTP %d", resp.StatusCode)
	}

	var ms struct {
		XMLName   xml.Name            `xml:"DAV: multistatus"`
		Responses []appleSyncResponse `xml:"DAV: response"`
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("PROPFIND parse: %w", err)
	}
	return ms.Responses, nil
}

func convertEvent(ev ical.Event, calendarID, path string) calendar.Event {
	summary, _ := ev.Props.Text(ical.PropSummary)
	desc, _ := ev.Props.Text(ical.PropDescription)
	loc, _ := ev.Props.Text(ical.PropLocation)
	dtStart, _ := ev.DateTimeStart(nil)
	dtEnd, _ := ev.DateTimeEnd(nil)

	return calendar.Event{
		ID:          path,
		CalendarID:  calendarID,
		Title:       summary,
		Description: desc,
		Location:    loc,
		Start:       dtStart,
		End:         dtEnd,
		AllDay:      appleEventAllDay(ev),
		Attendees:   parseAttendees(ev),
	}
}
