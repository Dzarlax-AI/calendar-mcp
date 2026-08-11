package calendar

import (
	"bytes"
	"encoding/json"
)

type PatchField[T any] struct {
	Present bool `json:"-"`
	Null    bool `json:"-"`
	Value   T    `json:"-"`
}

func (f *PatchField[T]) UnmarshalJSON(data []byte) error {
	f.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		f.Null = true
		var zero T
		f.Value = zero
		return nil
	}
	f.Null = false
	return json.Unmarshal(data, &f.Value)
}

type EventPatchV2 struct {
	Title            PatchField[string]               `json:"title"`
	Description      PatchField[string]               `json:"description"`
	Location         PatchField[string]               `json:"location"`
	Start            PatchField[EventTime]            `json:"start"`
	End              PatchField[EventTime]            `json:"end"`
	Recurrence       PatchField[[]string]             `json:"recurrence"`
	Attendees        PatchField[[]AttendeeV2]         `json:"attendees"`
	Reminders        PatchField[ReminderSettings]     `json:"reminders"`
	Visibility       PatchField[string]               `json:"visibility"`
	Transparency     PatchField[string]               `json:"transparency"`
	ColorID          PatchField[string]               `json:"color_id"`
	Attachments      PatchField[[]Attachment]         `json:"attachments"`
	Conference       PatchField[ConferenceData]       `json:"conference"`
	GuestPermissions PatchField[GuestPermissions]     `json:"guest_permissions"`
	Google           PatchField[GoogleEventExtension] `json:"google"`
}
