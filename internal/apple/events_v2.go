package apple

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

func (p *Provider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{
		Operations:           calendar.OperationCapabilities{List: true, Get: true, Create: true, Update: true, Delete: true},
		Fields:               calendar.FieldCapabilities{Recurrence: true},
		MutationScopes:       []calendar.MutationScope{calendar.ScopeSeries},
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
		EventTypes:           []string{"default"},
		Reasons: map[string]string{
			"instances":          "the Apple adapter does not expand recurrence locally yet",
			"attendees":          "CalDAV scheduling side effects cannot be suppressed reliably, so V2 attendee writes are rejected",
			"optimistic_locking": "go-webdav PutCalendarObject does not expose an If-Match precondition",
		},
	}, nil
}

func (p *Provider) ListEventsV2(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if request.View != calendar.RecurrenceSeries {
		return calendar.Page[calendar.EventV2]{}, unsupportedApple("Apple V2 currently supports recurrence view=series only")
	}
	query := &caldav.CalendarQuery{CompFilter: caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT", Start: request.Start, End: request.End}}}}
	objects, err := p.client.QueryCalendar(ctx, request.CalendarID, query)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	items := make([]calendar.EventV2, 0, len(objects))
	for _, object := range objects {
		for _, event := range object.Data.Events() {
			if event.Props.Get(ical.PropRecurrenceID) != nil {
				continue
			}
			items = append(items, appleEventV2(event, request.CalendarID, object.Path, object.ETag))
		}
	}
	return calendar.Page[calendar.EventV2]{Items: items, Complete: true}, nil
}

func (p *Provider) GetEventV2(ctx context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	path := appleObjectPath(ref.CalendarID, ref.EventID)
	objects, err := p.client.MultiGetCalendar(ctx, ref.CalendarID, &caldav.CalendarMultiGet{Paths: []string{path}})
	if err != nil || len(objects) == 0 {
		return nil, fmt.Errorf("event not found: %s", ref.EventID)
	}
	for _, event := range objects[0].Data.Events() {
		if event.Props.Get(ical.PropRecurrenceID) == nil {
			result := appleEventV2(event, ref.CalendarID, objects[0].Path, objects[0].ETag)
			return &result, nil
		}
	}
	return nil, fmt.Errorf("event has no master VEVENT: %s", ref.EventID)
}

func (p *Provider) CreateEventV2(ctx context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	if err := validateAppleWrite(request.Event); err != nil {
		return nil, err
	}
	uid := request.Event.ICalUID
	if uid == "" {
		uid = fmt.Sprintf("%d@calendar-mcp", time.Now().UnixNano())
	}
	event, err := appleEventFromCreate(uid, request.Event)
	if err != nil {
		return nil, invalidApple(err)
	}
	container := ical.NewCalendar()
	container.Props.SetText(ical.PropVersion, "2.0")
	container.Props.SetText(ical.PropProductID, "-//calendar-mcp//EN")
	container.Children = append(container.Children, event.Component)
	path := appleObjectPath(request.CalendarID, uid)
	object, err := p.client.PutCalendarObject(ctx, path, container)
	if err != nil {
		return nil, err
	}
	result := appleEventV2(*event, request.CalendarID, object.Path, object.ETag)
	return &result, nil
}

func (p *Provider) UpdateEventV2(ctx context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if request.Scope != calendar.ScopeSeries {
		return nil, unsupportedApple("Apple V2 supports series mutations only")
	}
	if request.ExpectedETag != "" {
		return nil, unsupportedApple("Apple V2 optimistic locking is unavailable")
	}
	if request.Patch.Attendees.Present || request.Patch.Reminders.Present || request.Patch.Attachments.Present || request.Patch.ColorID.Present || request.Patch.Conference.Present || request.Patch.GuestPermissions.Present || request.Patch.Google.Present {
		return nil, unsupportedApple("the patch contains fields not supported safely by Apple V2")
	}
	path := appleObjectPath(request.Ref.CalendarID, request.Ref.EventID)
	objects, err := p.client.MultiGetCalendar(ctx, request.Ref.CalendarID, &caldav.CalendarMultiGet{Paths: []string{path}})
	if err != nil || len(objects) == 0 {
		return nil, fmt.Errorf("event not found: %s", request.Ref.EventID)
	}
	var master *ical.Event
	for _, value := range objects[0].Data.Events() {
		if value.Props.Get(ical.PropRecurrenceID) == nil {
			copy := value
			master = &copy
			break
		}
	}
	if master == nil {
		return nil, fmt.Errorf("event has no master VEVENT")
	}
	applyAppleTextPatch(master, ical.PropSummary, request.Patch.Title)
	applyAppleTextPatch(master, ical.PropDescription, request.Patch.Description)
	applyAppleTextPatch(master, ical.PropLocation, request.Patch.Location)
	applyAppleTextPatch(master, ical.PropClass, request.Patch.Visibility)
	applyAppleTextPatch(master, ical.PropTransparency, request.Patch.Transparency)
	if request.Patch.Start.Present {
		if request.Patch.Start.Null {
			return nil, invalidApple(fmt.Errorf("start cannot be null"))
		}
		if err := setAppleEventTimeV2(master, ical.PropDateTimeStart, request.Patch.Start.Value); err != nil {
			return nil, invalidApple(err)
		}
	}
	if request.Patch.End.Present {
		if request.Patch.End.Null {
			return nil, invalidApple(fmt.Errorf("end cannot be null"))
		}
		if err := setAppleEventTimeV2(master, ical.PropDateTimeEnd, request.Patch.End.Value); err != nil {
			return nil, invalidApple(err)
		}
	}
	if request.Patch.Recurrence.Present {
		if err := setAppleRecurrence(master, request.Patch.Recurrence.Value); err != nil {
			return nil, invalidApple(err)
		}
	}
	object, err := p.client.PutCalendarObject(ctx, path, objects[0].Data)
	if err != nil {
		return nil, err
	}
	result := appleEventV2(*master, request.Ref.CalendarID, object.Path, object.ETag)
	return &calendar.OperationResult{Status: "completed", Event: &result}, nil
}

func (p *Provider) DeleteEventV2(ctx context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if request.Scope != calendar.ScopeSeries {
		return nil, unsupportedApple("Apple V2 supports series mutations only")
	}
	if request.ExpectedETag != "" {
		return nil, unsupportedApple("Apple V2 optimistic locking is unavailable")
	}
	if err := p.client.RemoveAll(ctx, appleObjectPath(request.Ref.CalendarID, request.Ref.EventID)); err != nil {
		return nil, err
	}
	return &calendar.OperationResult{Status: "completed"}, nil
}

func validateAppleWrite(event calendar.EventCreateV2) error {
	if len(event.Attendees) > 0 {
		return unsupportedApple("Apple V2 attendee writes are disabled because scheduling notifications cannot be suppressed reliably")
	}
	if event.Reminders != nil || len(event.Attachments) > 0 || event.ColorID != "" || event.Conference != nil || event.GuestPermissions != nil || event.Google != nil {
		return unsupportedApple("the event contains fields not supported safely by Apple V2")
	}
	return nil
}

func appleEventFromCreate(uid string, input calendar.EventCreateV2) (*ical.Event, error) {
	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid)
	event.Props.SetText(ical.PropSummary, input.Title)
	if err := setAppleEventTimeV2(event, ical.PropDateTimeStart, input.Start); err != nil {
		return nil, err
	}
	if err := setAppleEventTimeV2(event, ical.PropDateTimeEnd, input.End); err != nil {
		return nil, err
	}
	if input.Description != "" {
		event.Props.SetText(ical.PropDescription, input.Description)
	}
	if input.Location != "" {
		event.Props.SetText(ical.PropLocation, input.Location)
	}
	if input.Visibility != "" {
		event.Props.SetText(ical.PropClass, input.Visibility)
	}
	if input.Transparency != "" {
		event.Props.SetText(ical.PropTransparency, input.Transparency)
	}
	if err := setAppleRecurrence(event, input.Recurrence); err != nil {
		return nil, err
	}
	return event, nil
}

func setAppleEventTimeV2(event *ical.Event, name string, value calendar.EventTime) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if value.IsAllDay() {
		parsed, _ := time.Parse(calendar.DateLayout, value.Date)
		event.Props.SetDate(name, parsed)
		return nil
	}
	parsed, _ := value.Instant()
	location, _ := time.LoadLocation(value.TimeZone)
	event.Props.SetDateTime(name, parsed.In(location))
	prop := event.Props.Get(name)
	if value.TimeZone != "UTC" {
		prop.Params.Set("TZID", value.TimeZone)
	}
	return nil
}

func setAppleRecurrence(event *ical.Event, lines []string) error {
	if err := calendar.ValidateRecurrence(lines); err != nil {
		return err
	}
	for _, name := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates} {
		event.Props.Del(name)
	}
	for _, line := range lines {
		name, value, _ := strings.Cut(line, ":")
		property := strings.SplitN(strings.ToUpper(name), ";", 2)[0]
		prop := ical.NewProp(property)
		prop.Value = value
		event.Props.Add(prop)
	}
	return nil
}

func appleEventV2(event ical.Event, calendarID, path, etag string) calendar.EventV2 {
	uid, _ := event.Props.Text(ical.PropUID)
	title, _ := event.Props.Text(ical.PropSummary)
	description, _ := event.Props.Text(ical.PropDescription)
	location, _ := event.Props.Text(ical.PropLocation)
	visibility, _ := event.Props.Text(ical.PropClass)
	transparency, _ := event.Props.Text(ical.PropTransparency)
	status, _ := event.Props.Text(ical.PropStatus)
	result := calendar.EventV2{ID: path, CalendarID: calendarID, ICalUID: uid, ETag: etag, Title: title, Description: description, Location: location, Visibility: visibility, Transparency: transparency, Status: status, Start: appleEventTimeV2(event.Props.Get(ical.PropDateTimeStart)), End: appleEventTimeV2(event.Props.Get(ical.PropDateTimeEnd))}
	for _, name := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates} {
		for _, prop := range event.Props.Values(name) {
			result.Recurrence = append(result.Recurrence, name+":"+prop.Value)
		}
	}
	for _, attendee := range parseAttendees(event) {
		result.Attendees = append(result.Attendees, calendar.AttendeeV2{PersonV2: calendar.PersonV2{Email: attendee.Email, Name: attendee.Name}, Status: attendee.Status, Optional: attendee.Optional})
	}
	return result
}

func appleEventTimeV2(prop *ical.Prop) calendar.EventTime {
	if prop == nil {
		return calendar.EventTime{}
	}
	value, err := prop.DateTime(nil)
	if err != nil {
		return calendar.EventTime{}
	}
	if prop.ValueType() == ical.ValueDate {
		return calendar.EventTime{Date: value.Format(calendar.DateLayout)}
	}
	zone := prop.Params.Get("TZID")
	if zone == "" {
		zone = "UTC"
	}
	if location, err := time.LoadLocation(zone); err == nil {
		value = value.In(location)
	}
	return calendar.EventTime{DateTime: value.Format(time.RFC3339), TimeZone: zone}
}
func applyAppleTextPatch(event *ical.Event, name string, patch calendar.PatchField[string]) {
	if !patch.Present {
		return
	}
	if patch.Null || patch.Value == "" {
		event.Props.Del(name)
	} else {
		event.Props.SetText(name, patch.Value)
	}
}
func invalidApple(err error) *calendar.APIError {
	return &calendar.APIError{Code: calendar.ErrorInvalidArgument, Message: err.Error(), Provider: "apple", Cause: err}
}
func unsupportedApple(message string) *calendar.APIError {
	return &calendar.APIError{Code: calendar.ErrorUnsupportedCapability, Message: message, Provider: "apple"}
}

var _ calendar.EventProviderV2 = (*Provider)(nil)
