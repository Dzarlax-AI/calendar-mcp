package google

import (
	"context"
	"fmt"
	"time"

	gcal "google.golang.org/api/calendar/v3"

	"calendar-mcp/internal/calendar"
)

func (p *Provider) SearchEventsV2(ctx context.Context, request calendar.SearchEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	call := p.svc.Events.List(request.CalendarID).Q(request.Query).ShowDeleted(request.ShowDeleted).
		SingleEvents(true).OrderBy("startTime").MaxResults(googlePageSize(request.MaxResults))
	if !request.Start.IsZero() {
		call = call.TimeMin(request.Start.Format(time.RFC3339))
	}
	if !request.End.IsZero() {
		call = call.TimeMax(request.End.Format(time.RFC3339))
	}
	if len(request.EventTypes) > 0 {
		call = call.EventTypes(request.EventTypes...)
	}
	if request.PageToken != "" {
		call = call.PageToken(request.PageToken)
	}
	response, err := call.Context(ctx).Do()
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	items := make([]calendar.EventV2, 0, len(response.Items))
	for _, event := range response.Items {
		items = append(items, fromGoogleEventV2(event, request.CalendarID, response.TimeZone))
	}
	return calendar.Page[calendar.EventV2]{Items: items, NextPageToken: response.NextPageToken, Complete: true}, nil
}

func (p *Provider) RespondToEventV2(ctx context.Context, request calendar.RespondToEventRequestV2) (*calendar.OperationResult, error) {
	responseStatus, err := googleResponseStatus(request.Response)
	if err != nil {
		return nil, err
	}
	sendUpdates, err := googleSendUpdates(request.Notifications)
	if err != nil {
		return nil, err
	}
	existing, err := p.svc.Events.Get(request.Ref.CalendarID, request.Ref.EventID).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var self *gcal.EventAttendee
	for _, attendee := range existing.Attendees {
		if attendee.Self {
			copy := *attendee
			self = &copy
			break
		}
	}
	if self == nil {
		return nil, invalidGoogleArgument(fmt.Errorf("the authenticated user is not an attendee of this event"))
	}
	self.ResponseStatus = responseStatus
	self.Comment = request.Comment
	patch := &gcal.Event{Attendees: []*gcal.EventAttendee{self}, AttendeesOmitted: true}
	patch.ForceSendFields = append(patch.ForceSendFields, "AttendeesOmitted")
	call := p.svc.Events.Patch(request.Ref.CalendarID, request.Ref.EventID, patch).SendUpdates(sendUpdates)
	if request.ExpectedETag != "" {
		call.Header().Set("If-Match", request.ExpectedETag)
	}
	updated, err := call.Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	result := fromGoogleEventV2(updated, request.Ref.CalendarID, eventTimeZone(existing.Start))
	return &calendar.OperationResult{Status: "completed", Event: &result}, nil
}

func (p *Provider) MoveEventV2(ctx context.Context, request calendar.MoveEventRequestV2) (*calendar.OperationResult, error) {
	if request.DestinationCalendarID == "" {
		return nil, invalidGoogleArgument(fmt.Errorf("destination_calendar_id is required"))
	}
	sendUpdates, err := googleSendUpdates(request.Notifications)
	if err != nil {
		return nil, err
	}
	existing, err := p.svc.Events.Get(request.Ref.CalendarID, request.Ref.EventID).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	if normalizedGoogleEventType(existing.EventType) != "default" {
		return nil, unsupportedGoogle("Google can move only default events")
	}
	call := p.svc.Events.Move(request.Ref.CalendarID, request.Ref.EventID, request.DestinationCalendarID).SendUpdates(sendUpdates)
	if request.ExpectedETag != "" {
		call.Header().Set("If-Match", request.ExpectedETag)
	}
	moved, err := call.Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	result := fromGoogleEventV2(moved, request.DestinationCalendarID, eventTimeZone(existing.Start))
	return &calendar.OperationResult{Status: "completed", Event: &result}, nil
}

func (p *Provider) ImportEventV2(ctx context.Context, request calendar.ImportEventRequestV2) (*calendar.EventV2, error) {
	if request.Event.ICalUID == "" {
		return nil, invalidGoogleArgument(fmt.Errorf("ical_uid is required for Google event import"))
	}
	event, err := toGoogleEventV2(request.Event)
	if err != nil {
		return nil, invalidGoogleArgument(err)
	}
	if normalizedGoogleEventType(event.EventType) != "default" {
		return nil, unsupportedGoogle("Google can import only default events")
	}
	call := p.svc.Events.Import(request.CalendarID, event)
	if len(event.Attachments) > 0 {
		call = call.SupportsAttachments(true)
	}
	if event.ConferenceData != nil {
		call = call.ConferenceDataVersion(1)
	}
	imported, err := call.Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	result := fromGoogleEventV2(imported, request.CalendarID, eventTimeZone(event.Start))
	return &result, nil
}

func (p *Provider) FindEventByOperationIDV2(ctx context.Context, calendarID, operationID string) (*calendar.EventV2, error) {
	response, err := p.svc.Events.List(calendarID).
		PrivateExtendedProperty("calendarMcpOperation=" + operationID).
		ShowDeleted(false).
		SingleEvents(false).
		MaxResults(2).
		Context(ctx).
		Do()
	if err != nil {
		return nil, err
	}
	if len(response.Items) == 0 {
		return nil, nil
	}
	if len(response.Items) > 1 {
		return nil, &calendar.APIError{Code: calendar.ErrorConflict, Message: "multiple replacement series share the same operation marker", Provider: "google"}
	}
	result := fromGoogleEventV2(response.Items[0], calendarID, response.TimeZone)
	return &result, nil
}

func googleResponseStatus(value string) (string, error) {
	switch value {
	case "accepted", "declined", "tentative", "needsAction":
		return value, nil
	case "accept":
		return "accepted", nil
	case "decline":
		return "declined", nil
	default:
		return "", invalidGoogleArgument(fmt.Errorf("response must be accepted, declined, tentative, or needsAction"))
	}
}

var _ calendar.SearchProviderV2 = (*Provider)(nil)
var _ calendar.ResponseProviderV2 = (*Provider)(nil)
var _ calendar.MoveProviderV2 = (*Provider)(nil)
var _ calendar.ImportProviderV2 = (*Provider)(nil)
var _ calendar.FollowingLookupProviderV2 = (*Provider)(nil)
