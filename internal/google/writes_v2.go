package google

import (
	"context"
	"fmt"
	"strings"
	"time"

	gcal "google.golang.org/api/calendar/v3"

	"calendar-mcp/internal/calendar"
)

func (p *Provider) CreateEventV2(ctx context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	event, err := toGoogleEventV2(request.Event)
	if err != nil {
		return nil, invalidGoogleArgument(err)
	}
	if err := validateGoogleEventType(event, true); err != nil {
		return nil, invalidGoogleArgument(err)
	}
	sendUpdates, err := googleSendUpdates(request.Notifications)
	if err != nil {
		return nil, err
	}
	call := p.svc.Events.Insert(request.CalendarID, event).SendUpdates(sendUpdates)
	if len(event.Attachments) > 0 {
		call = call.SupportsAttachments(true)
	}
	if event.ConferenceData != nil {
		call = call.ConferenceDataVersion(1)
	}
	created, err := call.Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	result := fromGoogleEventV2(created, request.CalendarID, eventTimeZone(event.Start))
	return &result, nil
}

func (p *Provider) UpdateEventV2(ctx context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if request.Scope == calendar.ScopeFollowing {
		return nil, unsupportedGoogle("following updates require the recoverable split-series workflow")
	}
	sendUpdates, err := googleSendUpdates(request.Notifications)
	if err != nil {
		return nil, err
	}
	existing, err := p.svc.Events.Get(request.Ref.CalendarID, request.Ref.EventID).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	if err := applyGooglePatch(existing, request.Patch); err != nil {
		return nil, invalidGoogleArgument(err)
	}
	if err := validateGoogleEventType(existing, false); err != nil {
		return nil, invalidGoogleArgument(err)
	}

	call := p.svc.Events.Update(request.Ref.CalendarID, request.Ref.EventID, existing).SendUpdates(sendUpdates)
	if request.ExpectedETag != "" {
		call.Header().Set("If-Match", request.ExpectedETag)
	}
	if request.Patch.Attachments.Present || len(existing.Attachments) > 0 {
		call = call.SupportsAttachments(true)
	}
	if request.Patch.Conference.Present || existing.ConferenceData != nil {
		call = call.ConferenceDataVersion(1)
	}
	updated, err := call.Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	result := fromGoogleEventV2(updated, request.Ref.CalendarID, eventTimeZone(existing.Start))
	return &calendar.OperationResult{Status: "completed", Event: &result}, nil
}

func (p *Provider) DeleteEventV2(ctx context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if request.Scope == calendar.ScopeFollowing {
		return nil, unsupportedGoogle("following deletes require the recoverable split-series workflow")
	}
	sendUpdates, err := googleSendUpdates(request.Notifications)
	if err != nil {
		return nil, err
	}
	call := p.svc.Events.Delete(request.Ref.CalendarID, request.Ref.EventID).SendUpdates(sendUpdates)
	if request.ExpectedETag != "" {
		call.Header().Set("If-Match", request.ExpectedETag)
	}
	if err := call.Context(ctx).Do(); err != nil {
		return nil, err
	}
	return &calendar.OperationResult{Status: "completed"}, nil
}

func googleSendUpdates(policy calendar.NotificationPolicy) (string, error) {
	switch policy {
	case "", calendar.NotificationsNone:
		return "none", nil
	case calendar.NotificationsExternalOnly:
		return "externalOnly", nil
	case calendar.NotificationsAll:
		return "all", nil
	default:
		return "", invalidGoogleArgument(fmt.Errorf("unsupported notification policy %q", policy))
	}
}

func applyGooglePatch(event *gcal.Event, patch calendar.EventPatchV2) error {
	originalType := normalizedGoogleEventType(event.EventType)
	startChanged := patch.Start.Present
	endChanged := patch.End.Present

	applyStringPatch(&event.Summary, patch.Title, event, "Summary")
	applyStringPatch(&event.Description, patch.Description, event, "Description")
	applyStringPatch(&event.Location, patch.Location, event, "Location")
	applyStringPatch(&event.Visibility, patch.Visibility, event, "Visibility")
	applyStringPatch(&event.Transparency, patch.Transparency, event, "Transparency")
	applyStringPatch(&event.ColorId, patch.ColorID, event, "ColorId")

	if patch.Start.Present {
		if patch.Start.Null {
			return fmt.Errorf("start cannot be null")
		}
		value, err := toGoogleEventTimeV2(patch.Start.Value)
		if err != nil {
			return fmt.Errorf("invalid start: %w", err)
		}
		event.Start = value
	}
	if patch.End.Present {
		if patch.End.Null {
			return fmt.Errorf("end cannot be null")
		}
		value, err := toGoogleEventTimeV2(patch.End.Value)
		if err != nil {
			return fmt.Errorf("invalid end: %w", err)
		}
		event.End = value
	}
	if event.Start == nil || event.End == nil {
		return fmt.Errorf("start and end are required")
	}
	if startChanged != endChanged && event.Start.Date != "" != (event.End.Date != "") {
		return fmt.Errorf("changing between all-day and timed events requires both start and end")
	}
	start := fromGoogleEventTime(event.Start, eventTimeZone(event.Start))
	end := fromGoogleEventTime(event.End, eventTimeZone(event.End))
	if err := calendar.ValidateEventTimeRangeV2(start, end); err != nil {
		return err
	}

	if patch.Recurrence.Present {
		if patch.Recurrence.Null {
			event.Recurrence = nil
			event.NullFields = appendField(event.NullFields, "Recurrence")
		} else {
			if err := calendar.ValidateRecurrence(patch.Recurrence.Value); err != nil {
				return err
			}
			event.Recurrence = append([]string(nil), patch.Recurrence.Value...)
			if len(event.Recurrence) == 0 {
				event.ForceSendFields = appendField(event.ForceSendFields, "Recurrence")
			}
		}
	}
	if patch.Attendees.Present {
		if patch.Attendees.Null {
			event.Attendees = nil
			event.NullFields = appendField(event.NullFields, "Attendees")
		} else {
			event.Attendees = toGoogleAttendeesV2(patch.Attendees.Value)
			if len(event.Attendees) == 0 {
				event.ForceSendFields = appendField(event.ForceSendFields, "Attendees")
			}
		}
	}
	if patch.Reminders.Present {
		if patch.Reminders.Null {
			event.Reminders = nil
			event.NullFields = appendField(event.NullFields, "Reminders")
		} else {
			event.Reminders = toGoogleReminders(&patch.Reminders.Value)
		}
	}
	if patch.Attachments.Present {
		if patch.Attachments.Null {
			event.Attachments = nil
			event.NullFields = appendField(event.NullFields, "Attachments")
		} else {
			event.Attachments = toGoogleAttachments(patch.Attachments.Value)
			if len(event.Attachments) == 0 {
				event.ForceSendFields = appendField(event.ForceSendFields, "Attachments")
			}
		}
	}
	if patch.Conference.Present {
		if patch.Conference.Null {
			event.ConferenceData = nil
			event.NullFields = appendField(event.NullFields, "ConferenceData")
		} else {
			event.ConferenceData = toGoogleConference(&patch.Conference.Value)
		}
	}
	if patch.GuestPermissions.Present {
		if patch.GuestPermissions.Null {
			event.GuestsCanInviteOthers = nil
			event.GuestsCanModify = false
			event.GuestsCanSeeOtherGuests = nil
			event.NullFields = appendField(event.NullFields, "GuestsCanInviteOthers")
			event.ForceSendFields = appendField(event.ForceSendFields, "GuestsCanModify")
			event.NullFields = appendField(event.NullFields, "GuestsCanSeeOtherGuests")
		} else {
			value := patch.GuestPermissions.Value
			event.GuestsCanInviteOthers = boolPointer(value.CanInviteOthers)
			event.GuestsCanModify = value.CanModify
			event.GuestsCanSeeOtherGuests = boolPointer(value.CanSeeOthers)
			event.ForceSendFields = appendField(event.ForceSendFields, "GuestsCanModify")
		}
	}
	if patch.Google.Present {
		if patch.Google.Null {
			event.ExtendedProperties = nil
			event.NullFields = appendField(event.NullFields, "ExtendedProperties")
		} else {
			value := patch.Google.Value
			if value.EventType != "" && normalizedGoogleEventType(value.EventType) != originalType {
				return fmt.Errorf("google event_type is immutable after creation")
			}
			event.ExtendedProperties = &gcal.EventExtendedProperties{Private: cloneMap(value.PrivateProperties), Shared: cloneMap(value.SharedProperties)}
			if err := decodeStructMap(value.Birthday, &event.BirthdayProperties); err != nil {
				return fmt.Errorf("birthday properties: %w", err)
			}
			if err := decodeStructMap(value.FocusTime, &event.FocusTimeProperties); err != nil {
				return fmt.Errorf("focus time properties: %w", err)
			}
			if err := decodeStructMap(value.OutOfOffice, &event.OutOfOfficeProperties); err != nil {
				return fmt.Errorf("out of office properties: %w", err)
			}
			if err := decodeStructMap(value.WorkingLocation, &event.WorkingLocationProperties); err != nil {
				return fmt.Errorf("working location properties: %w", err)
			}
		}
	}
	return nil
}

func applyStringPatch(target *string, patch calendar.PatchField[string], event *gcal.Event, field string) {
	if !patch.Present {
		return
	}
	*target = patch.Value
	if patch.Null {
		*target = ""
		event.NullFields = appendField(event.NullFields, field)
	} else if patch.Value == "" {
		event.ForceSendFields = appendField(event.ForceSendFields, field)
	}
}

func validateGoogleEventType(event *gcal.Event, creating bool) error {
	typeName := normalizedGoogleEventType(event.EventType)
	switch typeName {
	case "default":
		return nil
	case "fromGmail":
		if creating {
			return fmt.Errorf("fromGmail events cannot be created")
		}
		return nil
	case "birthday":
		if event.Start == nil || event.End == nil || event.Start.Date == "" || event.End.Date == "" {
			return fmt.Errorf("birthday events must be all-day")
		}
		start, startErr := time.Parse(calendar.DateLayout, event.Start.Date)
		end, endErr := time.Parse(calendar.DateLayout, event.End.Date)
		if startErr != nil || endErr != nil || !end.Equal(start.AddDate(0, 0, 1)) {
			return fmt.Errorf("birthday events must span exactly one day")
		}
		if !hasYearlyRule(event.Recurrence) {
			return fmt.Errorf("birthday events require an annual RRULE")
		}
		if event.Visibility != "" && event.Visibility != "private" {
			return fmt.Errorf("birthday events must use private visibility")
		}
		if event.Transparency != "" && event.Transparency != "transparent" {
			return fmt.Errorf("birthday events must be transparent")
		}
	case "focusTime", "outOfOffice":
		if event.Start == nil || event.End == nil || event.Start.DateTime == "" || event.End.DateTime == "" {
			return fmt.Errorf("%s events must be timed", typeName)
		}
		if event.Transparency != "" && event.Transparency != "opaque" {
			return fmt.Errorf("%s events must be opaque", typeName)
		}
	case "workingLocation":
		if event.Start == nil || event.End == nil {
			return fmt.Errorf("workingLocation events require start and end")
		}
		if event.Start.Date != "" {
			start, startErr := time.Parse(calendar.DateLayout, event.Start.Date)
			end, endErr := time.Parse(calendar.DateLayout, event.End.Date)
			if startErr != nil || endErr != nil || !end.Equal(start.AddDate(0, 0, 1)) {
				return fmt.Errorf("all-day workingLocation events must span exactly one day")
			}
		}
		if event.Visibility != "" && event.Visibility != "public" {
			return fmt.Errorf("workingLocation events must use public visibility")
		}
		if event.Transparency != "" && event.Transparency != "transparent" {
			return fmt.Errorf("workingLocation events must be transparent")
		}
	default:
		return fmt.Errorf("unsupported google event_type %q", event.EventType)
	}
	return nil
}

func normalizedGoogleEventType(value string) string {
	if value == "" {
		return "default"
	}
	return value
}

func hasYearlyRule(recurrence []string) bool {
	for _, rule := range recurrence {
		upper := strings.ToUpper(rule)
		if strings.HasPrefix(upper, "RRULE:") && strings.Contains(upper, "FREQ=YEARLY") {
			return true
		}
	}
	return false
}

func eventTimeZone(value *gcal.EventDateTime) string {
	if value == nil {
		return ""
	}
	return value.TimeZone
}

func appendField(values []string, field string) []string {
	for _, value := range values {
		if value == field {
			return values
		}
	}
	return append(values, field)
}

func invalidGoogleArgument(err error) *calendar.APIError {
	return &calendar.APIError{Code: calendar.ErrorInvalidArgument, Message: err.Error(), Provider: "google", Cause: err}
}

func unsupportedGoogle(message string) *calendar.APIError {
	return &calendar.APIError{Code: calendar.ErrorUnsupportedCapability, Message: message, Provider: "google"}
}
