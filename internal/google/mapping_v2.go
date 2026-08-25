package google

import (
	"encoding/json"
	"fmt"
	"time"

	gcal "google.golang.org/api/calendar/v3"

	"calendar-mcp/internal/calendar"
)

func fromGoogleEventV2(event *gcal.Event, calendarID, fallbackTimeZone string) calendar.EventV2 {
	result := calendar.EventV2{
		ID:                event.Id,
		CalendarID:        calendarID,
		ICalUID:           event.ICalUID,
		ETag:              event.Etag,
		HTMLLink:          event.HtmlLink,
		Title:             event.Summary,
		Description:       event.Description,
		DescriptionFormat: "html", // Google Calendar event descriptions may contain HTML.
		Location:          event.Location,
		Status:            event.Status,
		RecurringEventID:  event.RecurringEventId,
		Recurrence:        append([]string(nil), event.Recurrence...),
		Attendees:         fromGoogleAttendeesV2(event.Attendees),
		Reminders:         fromGoogleReminders(event.Reminders),
		Visibility:        event.Visibility,
		Transparency:      event.Transparency,
		ColorID:           event.ColorId,
		Attachments:       fromGoogleAttachments(event.Attachments),
		Conference:        fromGoogleConference(event.ConferenceData),
		GuestPermissions: &calendar.GuestPermissions{
			CanInviteOthers: boolDefaultTrue(event.GuestsCanInviteOthers),
			CanModify:       event.GuestsCanModify,
			CanSeeOthers:    boolDefaultTrue(event.GuestsCanSeeOtherGuests),
		},
		Google: &calendar.GoogleEventExtension{
			EventType:       event.EventType,
			Locked:          event.Locked,
			PrivateCopy:     event.PrivateCopy,
			Birthday:        structMap(event.BirthdayProperties),
			FocusTime:       structMap(event.FocusTimeProperties),
			OutOfOffice:     structMap(event.OutOfOfficeProperties),
			WorkingLocation: structMap(event.WorkingLocationProperties),
		},
	}
	if event.ExtendedProperties != nil {
		result.Google.PrivateProperties = cloneMap(event.ExtendedProperties.Private)
		result.Google.SharedProperties = cloneMap(event.ExtendedProperties.Shared)
	}
	if event.Start != nil {
		result.Start = fromGoogleEventTime(event.Start, fallbackTimeZone)
	}
	if event.End != nil {
		result.End = fromGoogleEventTime(event.End, fallbackTimeZone)
	}
	if event.OriginalStartTime != nil {
		value := fromGoogleEventTime(event.OriginalStartTime, fallbackTimeZone)
		result.OriginalStart = &value
	}
	if event.RecurringEventId != "" {
		result.InstanceKind = "occurrence"
		if event.Status == "cancelled" {
			result.InstanceKind = "cancelled"
		}
	} else if len(event.Recurrence) > 0 {
		result.InstanceKind = "seriesMaster"
	}
	if event.Organizer != nil {
		result.Organizer = &calendar.PersonV2{ID: event.Organizer.Id, Email: event.Organizer.Email, Name: event.Organizer.DisplayName, Self: event.Organizer.Self}
	}
	if parsed, err := time.Parse(time.RFC3339, event.Created); err == nil {
		result.Created = &parsed
	}
	if parsed, err := time.Parse(time.RFC3339, event.Updated); err == nil {
		result.Updated = &parsed
	}
	return result
}

func fromGoogleEventTime(value *gcal.EventDateTime, fallbackTimeZone string) calendar.EventTime {
	if value.Date != "" {
		return calendar.EventTime{Date: value.Date}
	}
	zone := value.TimeZone
	if zone == "" {
		zone = fallbackTimeZone
	}
	parsed, err := time.Parse(time.RFC3339, value.DateTime)
	if err != nil {
		return calendar.EventTime{DateTime: value.DateTime, TimeZone: zone}
	}
	if zone != "" {
		if location, err := time.LoadLocation(zone); err == nil {
			return calendar.EventTime{DateTime: parsed.In(location).Format(time.RFC3339), TimeZone: zone}
		}
	}
	return calendar.EventTime{DateTime: parsed.UTC().Format(time.RFC3339), TimeZone: "UTC"}
}

func toGoogleEventTimeV2(value calendar.EventTime) (*gcal.EventDateTime, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	if value.IsAllDay() {
		return &gcal.EventDateTime{Date: value.Date}, nil
	}
	return &gcal.EventDateTime{DateTime: value.DateTime, TimeZone: value.TimeZone}, nil
}

func toGoogleEventV2(input calendar.EventCreateV2) (*gcal.Event, error) {
	if err := calendar.ValidateEventTimeRangeV2(input.Start, input.End); err != nil {
		return nil, err
	}
	if err := calendar.ValidateRecurrence(input.Recurrence); err != nil {
		return nil, err
	}
	start, _ := toGoogleEventTimeV2(input.Start)
	end, _ := toGoogleEventTimeV2(input.End)
	event := &gcal.Event{
		ICalUID:        input.ICalUID,
		Summary:        input.Title,
		Description:    input.Description,
		Location:       input.Location,
		Start:          start,
		End:            end,
		Recurrence:     append([]string(nil), input.Recurrence...),
		Attendees:      toGoogleAttendeesV2(input.Attendees),
		Reminders:      toGoogleReminders(input.Reminders),
		Visibility:     input.Visibility,
		Transparency:   input.Transparency,
		ColorId:        input.ColorID,
		Attachments:    toGoogleAttachments(input.Attachments),
		ConferenceData: toGoogleConference(input.Conference),
	}
	if input.GuestPermissions != nil {
		event.GuestsCanInviteOthers = boolPointer(input.GuestPermissions.CanInviteOthers)
		event.GuestsCanModify = input.GuestPermissions.CanModify
		event.GuestsCanSeeOtherGuests = boolPointer(input.GuestPermissions.CanSeeOthers)
	}
	if input.Google != nil {
		event.EventType = input.Google.EventType
		event.ExtendedProperties = &gcal.EventExtendedProperties{
			Private: cloneMap(input.Google.PrivateProperties),
			Shared:  cloneMap(input.Google.SharedProperties),
		}
		if err := decodeStructMap(input.Google.Birthday, &event.BirthdayProperties); err != nil {
			return nil, fmt.Errorf("birthday properties: %w", err)
		}
		if err := decodeStructMap(input.Google.FocusTime, &event.FocusTimeProperties); err != nil {
			return nil, fmt.Errorf("focus time properties: %w", err)
		}
		if err := decodeStructMap(input.Google.OutOfOffice, &event.OutOfOfficeProperties); err != nil {
			return nil, fmt.Errorf("out of office properties: %w", err)
		}
		if err := decodeStructMap(input.Google.WorkingLocation, &event.WorkingLocationProperties); err != nil {
			return nil, fmt.Errorf("working location properties: %w", err)
		}
	}
	if input.SyncMarker != nil {
		if event.ExtendedProperties == nil {
			event.ExtendedProperties = &gcal.EventExtendedProperties{}
		}
		if event.ExtendedProperties.Private == nil {
			event.ExtendedProperties.Private = map[string]string{}
		}
		event.ExtendedProperties.Private["calendar_sync_rule"] = input.SyncMarker.RuleID
		event.ExtendedProperties.Private["calendar_source_event"] = input.SyncMarker.SourceEventID
	}
	return event, nil
}

func fromGoogleAttendeesV2(values []*gcal.EventAttendee) []calendar.AttendeeV2 {
	result := make([]calendar.AttendeeV2, 0, len(values))
	for _, value := range values {
		result = append(result, calendar.AttendeeV2{
			PersonV2: calendar.PersonV2{ID: value.Id, Email: value.Email, Name: value.DisplayName, Self: value.Self, Comment: value.Comment},
			Status:   value.ResponseStatus, Optional: value.Optional, Organizer: value.Organizer,
			Resource: value.Resource, AdditionalGuests: value.AdditionalGuests,
		})
	}
	return result
}

func toGoogleAttendeesV2(values []calendar.AttendeeV2) []*gcal.EventAttendee {
	result := make([]*gcal.EventAttendee, 0, len(values))
	for _, value := range values {
		result = append(result, &gcal.EventAttendee{
			Id: value.ID, Email: value.Email, DisplayName: value.Name, Comment: value.Comment,
			ResponseStatus: value.Status, Optional: value.Optional, Resource: value.Resource,
			AdditionalGuests: value.AdditionalGuests,
		})
	}
	return result
}

func fromGoogleReminders(value *gcal.EventReminders) *calendar.ReminderSettings {
	if value == nil {
		return nil
	}
	result := &calendar.ReminderSettings{UseDefault: value.UseDefault}
	for _, reminder := range value.Overrides {
		result.Overrides = append(result.Overrides, calendar.Reminder{Method: reminder.Method, Minutes: reminder.Minutes})
	}
	return result
}

func toGoogleReminders(value *calendar.ReminderSettings) *gcal.EventReminders {
	if value == nil {
		return nil
	}
	result := &gcal.EventReminders{UseDefault: value.UseDefault}
	for _, reminder := range value.Overrides {
		result.Overrides = append(result.Overrides, &gcal.EventReminder{Method: reminder.Method, Minutes: reminder.Minutes})
	}
	if !value.UseDefault && len(value.Overrides) == 0 {
		result.ForceSendFields = append(result.ForceSendFields, "Overrides")
	}
	return prepareGoogleRemindersForWrite(result)
}

func prepareGoogleRemindersForWrite(value *gcal.EventReminders) *gcal.EventReminders {
	if value != nil && !value.UseDefault {
		value.ForceSendFields = appendField(value.ForceSendFields, "UseDefault")
	}
	return value
}

func fromGoogleAttachments(values []*gcal.EventAttachment) []calendar.Attachment {
	result := make([]calendar.Attachment, 0, len(values))
	for _, value := range values {
		result = append(result, calendar.Attachment{FileURL: value.FileUrl, Title: value.Title, MimeType: value.MimeType, IconLink: value.IconLink, FileID: value.FileId})
	}
	return result
}

func toGoogleAttachments(values []calendar.Attachment) []*gcal.EventAttachment {
	result := make([]*gcal.EventAttachment, 0, len(values))
	for _, value := range values {
		result = append(result, &gcal.EventAttachment{FileUrl: value.FileURL, Title: value.Title, MimeType: value.MimeType, IconLink: value.IconLink, FileId: value.FileID})
	}
	return result
}

func fromGoogleConference(value *gcal.ConferenceData) *calendar.ConferenceData {
	if value == nil {
		return nil
	}
	result := &calendar.ConferenceData{}
	if value.CreateRequest != nil {
		result.RequestID = value.CreateRequest.RequestId
		if value.CreateRequest.Status != nil {
			result.Status = value.CreateRequest.Status.StatusCode
		}
	}
	if value.ConferenceSolution != nil {
		result.Solution = value.ConferenceSolution.Name
		if value.ConferenceSolution.Key != nil && result.Solution == "" {
			result.Solution = value.ConferenceSolution.Key.Type
		}
	}
	for _, entry := range value.EntryPoints {
		result.EntryPoints = append(result.EntryPoints, calendar.ConferenceEntryPoint{Type: entry.EntryPointType, URI: entry.Uri, Label: entry.Label, PIN: entry.Pin})
	}
	return result
}

func toGoogleConference(value *calendar.ConferenceData) *gcal.ConferenceData {
	if value == nil {
		return nil
	}
	if value.RequestID != "" {
		return &gcal.ConferenceData{CreateRequest: &gcal.CreateConferenceRequest{
			RequestId:             value.RequestID,
			ConferenceSolutionKey: &gcal.ConferenceSolutionKey{Type: "hangoutsMeet"},
		}}
	}
	result := &gcal.ConferenceData{}
	for _, entry := range value.EntryPoints {
		result.EntryPoints = append(result.EntryPoints, &gcal.EntryPoint{EntryPointType: entry.Type, Uri: entry.URI, Label: entry.Label, Pin: entry.PIN})
	}
	return result
}

func boolDefaultTrue(value *bool) bool {
	return value == nil || *value
}

func boolPointer(value bool) *bool { return &value }

func cloneMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func structMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return result
}

func decodeStructMap(input map[string]any, target any) error {
	if input == nil {
		return nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
