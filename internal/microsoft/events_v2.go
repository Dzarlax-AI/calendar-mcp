package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"calendar-mcp/internal/calendar"
)

func (p *Provider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{
		Operations:           calendar.OperationCapabilities{List: true, Get: true, Create: true, Update: true, Delete: true},
		Fields:               calendar.FieldCapabilities{Conferencing: true, OptimisticLocking: true},
		MutationScopes:       []calendar.MutationScope{calendar.ScopeSeries, calendar.ScopeSingle},
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone, calendar.NotificationsAll},
		EventTypes:           []string{"default"},
		Reasons: map[string]string{
			"recurrence":    "Graph recurrence is readable through expanded calendarView, but portable V2 recurrence writes are not enabled yet",
			"notifications": "none is accepted only when the event has no attendees; Graph does not expose sendUpdates suppression",
		},
	}, nil
}

func (p *Provider) ListEventsV2(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if request.View != "" && request.View != calendar.RecurrenceExpanded {
		return calendar.Page[calendar.EventV2]{}, unsupportedMicrosoft("Microsoft V2 currently exposes expanded calendarView only")
	}
	path := fmt.Sprintf("/me/calendars/%s/calendarView", url.PathEscape(request.CalendarID))
	params := url.Values{"startDateTime": {request.Start.UTC().Format(time.RFC3339)}, "endDateTime": {request.End.UTC().Format(time.RFC3339)}, "$top": {strconv.FormatInt(msPageSize(request.MaxResults), 10)}}
	next := p.baseURL + path + "?" + params.Encode()
	if request.PageToken != "" {
		next = request.PageToken
	}
	var response struct {
		Value    []graphEvent `json:"value"`
		NextLink string       `json:"@odata.nextLink"`
	}
	if err := p.getURLWithHeaders(ctx, next, http.Header{"Prefer": {`outlook.timezone="UTC"`, `outlook.body-content-type="text"`}}, &response); err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	items := make([]calendar.EventV2, 0, len(response.Value))
	for i := range response.Value {
		items = append(items, response.Value[i].toEventV2(request.CalendarID))
	}
	return calendar.Page[calendar.EventV2]{Items: items, NextPageToken: response.NextLink, Complete: true}, nil
}

func (p *Provider) GetEventV2(ctx context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	path := fmt.Sprintf("/me/calendars/%s/events/%s", url.PathEscape(ref.CalendarID), url.PathEscape(ref.EventID))
	var event graphEvent
	if err := p.getWithParamsAndHeaders(ctx, path, nil, http.Header{"Prefer": {`outlook.timezone="UTC"`, `outlook.body-content-type="text"`}}, &event); err != nil {
		return nil, err
	}
	result := event.toEventV2(ref.CalendarID)
	return &result, nil
}

func (p *Provider) CreateEventV2(ctx context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	if err := validateMicrosoftCreate(request.Event, request.Notifications); err != nil {
		return nil, err
	}
	body, err := toGraphCreateV2(request.Event)
	if err != nil {
		return nil, invalidMicrosoft(err)
	}
	path := fmt.Sprintf("/me/calendars/%s/events", url.PathEscape(request.CalendarID))
	var created graphEvent
	if err := p.post(ctx, path, body, &created); err != nil {
		return nil, err
	}
	result := created.toEventV2(request.CalendarID)
	return &result, nil
}

func (p *Provider) UpdateEventV2(ctx context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if request.Scope == calendar.ScopeFollowing {
		return nil, unsupportedMicrosoft("following mutations are not supported by the Microsoft V2 adapter")
	}
	if request.Patch.Recurrence.Present || request.Patch.Reminders.Present || request.Patch.Attachments.Present || request.Patch.ColorID.Present || request.Patch.GuestPermissions.Present || request.Patch.Google.Present {
		return nil, unsupportedMicrosoft("the patch contains fields not supported by the Microsoft V2 adapter")
	}
	if request.Patch.Attendees.Present && request.Notifications != calendar.NotificationsAll {
		return nil, unsupportedMicrosoft("Microsoft attendee changes require notification_policy=all")
	}
	patch := map[string]any{}
	if request.Patch.Title.Present {
		patch["subject"] = request.Patch.Title.Value
	}
	if request.Patch.Description.Present {
		patch["body"] = graphBody{ContentType: "text", Content: request.Patch.Description.Value}
	}
	if request.Patch.Location.Present {
		patch["location"] = graphLocation{DisplayName: request.Patch.Location.Value}
	}
	if request.Patch.Start.Present {
		value, allDay, err := graphTimeFromEventTime(request.Patch.Start.Value)
		if err != nil {
			return nil, invalidMicrosoft(err)
		}
		patch["start"] = value
		patch["isAllDay"] = allDay
	}
	if request.Patch.End.Present {
		value, allDay, err := graphTimeFromEventTime(request.Patch.End.Value)
		if err != nil {
			return nil, invalidMicrosoft(err)
		}
		patch["end"] = value
		patch["isAllDay"] = allDay
	}
	if request.Patch.Attendees.Present {
		patch["attendees"] = toGraphAttendeesV2(request.Patch.Attendees.Value)
	}
	if request.Patch.Transparency.Present {
		showAs, err := graphShowAs(request.Patch.Transparency.Value)
		if err != nil {
			return nil, invalidMicrosoft(err)
		}
		patch["showAs"] = showAs
	}
	if request.Patch.Visibility.Present {
		patch["sensitivity"] = request.Patch.Visibility.Value
	}
	if request.Patch.Conference.Present {
		return nil, unsupportedMicrosoft("Teams meeting state cannot be safely changed after creation")
	}
	path := fmt.Sprintf("/me/calendars/%s/events/%s", url.PathEscape(request.Ref.CalendarID), url.PathEscape(request.Ref.EventID))
	var updated graphEvent
	if err := p.doJSONWithETag(ctx, http.MethodPatch, path, patch, request.ExpectedETag, &updated); err != nil {
		return nil, err
	}
	result := updated.toEventV2(request.Ref.CalendarID)
	return &calendar.OperationResult{Status: "completed", Event: &result}, nil
}

func (p *Provider) DeleteEventV2(ctx context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if request.Scope == calendar.ScopeFollowing {
		return nil, unsupportedMicrosoft("following mutations are not supported by the Microsoft V2 adapter")
	}
	if request.Notifications == calendar.NotificationsNone {
		event, err := p.GetEventV2(ctx, request.Ref)
		if err != nil {
			return nil, err
		}
		if len(event.Attendees) > 0 {
			return nil, unsupportedMicrosoft("Microsoft cannot suppress cancellation messages; use notification_policy=all")
		}
	}
	path := fmt.Sprintf("/me/calendars/%s/events/%s", url.PathEscape(request.Ref.CalendarID), url.PathEscape(request.Ref.EventID))
	if err := p.deleteWithETag(ctx, path, request.ExpectedETag); err != nil {
		return nil, err
	}
	return &calendar.OperationResult{Status: "completed"}, nil
}

func validateMicrosoftCreate(event calendar.EventCreateV2, notifications calendar.NotificationPolicy) error {
	if len(event.Recurrence) > 0 || event.Reminders != nil || len(event.Attachments) > 0 || event.ColorID != "" || event.GuestPermissions != nil || event.Google != nil {
		return unsupportedMicrosoft("the event contains fields not supported by the Microsoft V2 adapter")
	}
	if len(event.Attendees) > 0 && notifications != calendar.NotificationsAll {
		return unsupportedMicrosoft("Microsoft attendee creation requires notification_policy=all")
	}
	return nil
}

func toGraphCreateV2(event calendar.EventCreateV2) (graphEventCreate, error) {
	start, startAllDay, err := graphTimeFromEventTime(event.Start)
	if err != nil {
		return graphEventCreate{}, err
	}
	end, endAllDay, err := graphTimeFromEventTime(event.End)
	if err != nil {
		return graphEventCreate{}, err
	}
	if startAllDay != endAllDay {
		return graphEventCreate{}, fmt.Errorf("start and end must use the same representation")
	}
	body := graphEventCreate{Subject: event.Title, Body: &graphBody{ContentType: "text", Content: event.Description}, Start: start, End: end, IsAllDay: startAllDay, Attendees: toGraphAttendeesV2(event.Attendees)}
	if event.Location != "" {
		body.Location = &graphLocation{DisplayName: event.Location}
	}
	if event.Conference != nil {
		body.IsOnlineMeeting = true
		body.OnlineMeetingProvider = "teamsForBusiness"
	}
	return body, nil
}

func graphTimeFromEventTime(value calendar.EventTime) (graphDateTime, bool, error) {
	if err := value.Validate(); err != nil {
		return graphDateTime{}, false, err
	}
	if value.IsAllDay() {
		return graphDateTime{DateTime: value.Date + "T00:00:00", TimeZone: "UTC"}, true, nil
	}
	instant, _ := value.Instant()
	return toGraphDateTime(instant), false, nil
}

func toGraphAttendeesV2(values []calendar.AttendeeV2) []graphAttendee {
	legacy := make([]calendar.Attendee, 0, len(values))
	for _, value := range values {
		legacy = append(legacy, calendar.Attendee{Email: value.Email, Name: value.Name, Optional: value.Optional})
	}
	return toGraphAttendees(legacy)
}

func (e *graphEvent) toEventV2(calendarID string) calendar.EventV2 {
	result := calendar.EventV2{ID: e.ID, CalendarID: calendarID, ICalUID: e.ICalUID, ETag: e.ETag, Title: e.Subject, Description: e.Body.Content, Location: e.Location.DisplayName, RecurringEventID: e.SeriesMasterID, Transparency: graphTransparency(e.ShowAs), Visibility: e.Sensitivity}
	if e.IsAllDay {
		result.Start = calendar.EventTime{Date: graphDate(e.Start)}
		result.End = calendar.EventTime{Date: graphDate(e.End)}
	} else {
		result.Start = graphEventTime(e.Start)
		result.End = graphEventTime(e.End)
	}
	for _, attendee := range e.Attendees {
		status := ""
		if attendee.Status != nil {
			status = attendee.Status.Response
		}
		result.Attendees = append(result.Attendees, calendar.AttendeeV2{PersonV2: calendar.PersonV2{Email: attendee.EmailAddress.Address, Name: attendee.EmailAddress.Name}, Status: status, Optional: attendee.Type == "optional"})
	}
	if e.OnlineMeeting != nil {
		result.Conference = &calendar.ConferenceData{Solution: "teamsForBusiness", EntryPoints: []calendar.ConferenceEntryPoint{{Type: "video", URI: e.OnlineMeeting.JoinUrl}}}
	}
	return result
}

func graphEventTime(value graphDateTime) calendar.EventTime {
	instant := parseGraphTime(value)
	return calendar.EventTime{DateTime: instant.UTC().Format(time.RFC3339), TimeZone: "UTC"}
}
func graphDate(value graphDateTime) string {
	if len(value.DateTime) >= 10 {
		return value.DateTime[:10]
	}
	return ""
}
func graphTransparency(value string) string {
	if value == "free" {
		return "transparent"
	}
	return "opaque"
}
func graphShowAs(value string) (string, error) {
	switch value {
	case "", "opaque":
		return "busy", nil
	case "transparent":
		return "free", nil
	default:
		return "", fmt.Errorf("transparency must be opaque or transparent")
	}
}
func msPageSize(value int64) int64 {
	if value <= 0 {
		return 250
	}
	if value > 1000 {
		return 1000
	}
	return value
}

func (p *Provider) doJSONWithETag(ctx context.Context, method, path string, body any, etag string, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	return p.do(req, out)
}
func (p *Provider) deleteWithETag(ctx context.Context, path, etag string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.baseURL+path, nil)
	if err != nil {
		return err
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	return p.do(req, nil)
}
func invalidMicrosoft(err error) *calendar.APIError {
	return &calendar.APIError{Code: calendar.ErrorInvalidArgument, Message: err.Error(), Provider: "microsoft", Cause: err}
}
func unsupportedMicrosoft(message string) *calendar.APIError {
	return &calendar.APIError{Code: calendar.ErrorUnsupportedCapability, Message: message, Provider: "microsoft"}
}

var _ calendar.EventProviderV2 = (*Provider)(nil)
