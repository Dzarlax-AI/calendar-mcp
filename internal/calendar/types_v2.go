package calendar

import (
	"fmt"
	"time"
)

type EventTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"date_time,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
}

func (t EventTime) IsAllDay() bool { return t.Date != "" }

func (t EventTime) Validate() error {
	hasDate := t.Date != ""
	hasDateTime := t.DateTime != ""
	if hasDate == hasDateTime {
		return fmt.Errorf("exactly one of date or date_time is required")
	}
	if hasDate {
		if t.TimeZone != "" {
			return fmt.Errorf("time_zone is not allowed for an all-day date")
		}
		if _, err := time.Parse(DateLayout, t.Date); err != nil {
			return fmt.Errorf("invalid date: %w", err)
		}
		return nil
	}
	if t.TimeZone == "" {
		return fmt.Errorf("time_zone is required for date_time")
	}
	if t.TimeZone == "Local" {
		return fmt.Errorf("time_zone must be an explicit IANA name, not Local")
	}
	if _, err := time.LoadLocation(t.TimeZone); err != nil {
		return fmt.Errorf("invalid IANA time_zone %q: %w", t.TimeZone, err)
	}
	if _, err := time.Parse(time.RFC3339, t.DateTime); err != nil {
		return fmt.Errorf("invalid date_time: %w", err)
	}
	return nil
}

func (t EventTime) Instant() (time.Time, error) {
	if err := t.Validate(); err != nil {
		return time.Time{}, err
	}
	if t.IsAllDay() {
		return time.Parse(DateLayout, t.Date)
	}
	return time.Parse(time.RFC3339, t.DateTime)
}

func ValidateEventTimeRangeV2(start, end EventTime) error {
	if err := start.Validate(); err != nil {
		return fmt.Errorf("invalid start: %w", err)
	}
	if err := end.Validate(); err != nil {
		return fmt.Errorf("invalid end: %w", err)
	}
	if start.IsAllDay() != end.IsAllDay() {
		return fmt.Errorf("start and end must both be dates or both be date-times")
	}
	startTime, _ := start.Instant()
	endTime, _ := end.Instant()
	if !endTime.After(startTime) {
		return fmt.Errorf("end must be after start")
	}
	return nil
}

type PersonV2 struct {
	ID      string `json:"id,omitempty"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
	Self    bool   `json:"self,omitempty"`
	Comment string `json:"comment,omitempty"`
}

type AttendeeV2 struct {
	PersonV2
	Status           string `json:"status,omitempty"`
	Optional         bool   `json:"optional,omitempty"`
	Organizer        bool   `json:"organizer,omitempty"`
	Resource         bool   `json:"resource,omitempty"`
	AdditionalGuests int64  `json:"additional_guests,omitempty"`
}

type Reminder struct {
	Method  string `json:"method"`
	Minutes int64  `json:"minutes"`
}

type ReminderSettings struct {
	UseDefault bool       `json:"use_default"`
	Overrides  []Reminder `json:"overrides,omitempty"`
}

type Attachment struct {
	FileURL  string `json:"file_url"`
	Title    string `json:"title,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	IconLink string `json:"icon_link,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

type ConferenceEntryPoint struct {
	Type  string `json:"type"`
	URI   string `json:"uri"`
	Label string `json:"label,omitempty"`
	PIN   string `json:"pin,omitempty"`
}

type ConferenceData struct {
	RequestID   string                 `json:"request_id,omitempty"`
	Status      string                 `json:"status,omitempty"`
	Solution    string                 `json:"solution,omitempty"`
	EntryPoints []ConferenceEntryPoint `json:"entry_points,omitempty"`
}

type GuestPermissions struct {
	CanInviteOthers bool `json:"can_invite_others"`
	CanModify       bool `json:"can_modify"`
	CanSeeOthers    bool `json:"can_see_others"`
}

type GoogleEventExtension struct {
	EventType         string            `json:"event_type,omitempty"`
	PrivateProperties map[string]string `json:"private_properties,omitempty"`
	SharedProperties  map[string]string `json:"shared_properties,omitempty"`
	Birthday          map[string]any    `json:"birthday,omitempty"`
	FocusTime         map[string]any    `json:"focus_time,omitempty"`
	OutOfOffice       map[string]any    `json:"out_of_office,omitempty"`
	WorkingLocation   map[string]any    `json:"working_location,omitempty"`
	Locked            bool              `json:"locked,omitempty"`
	PrivateCopy       bool              `json:"private_copy,omitempty"`
}

type EventV2 struct {
	ID               string                `json:"id"`
	CalendarID       string                `json:"calendar_id"`
	Provider         string                `json:"provider"`
	ICalUID          string                `json:"ical_uid,omitempty"`
	ETag             string                `json:"etag,omitempty"`
	HTMLLink         string                `json:"html_link,omitempty"`
	Title            string                `json:"title,omitempty"`
	Description      string                `json:"description,omitempty"`
	Location         string                `json:"location,omitempty"`
	Status           string                `json:"status,omitempty"`
	Start            EventTime             `json:"start"`
	End              EventTime             `json:"end"`
	OriginalStart    *EventTime            `json:"original_start,omitempty"`
	RecurringEventID string                `json:"recurring_event_id,omitempty"`
	InstanceKind     string                `json:"instance_kind,omitempty"`
	Recurrence       []string              `json:"recurrence,omitempty"`
	Organizer        *PersonV2             `json:"organizer,omitempty"`
	Attendees        []AttendeeV2          `json:"attendees,omitempty"`
	Reminders        *ReminderSettings     `json:"reminders,omitempty"`
	Visibility       string                `json:"visibility,omitempty"`
	Transparency     string                `json:"transparency,omitempty"`
	ColorID          string                `json:"color_id,omitempty"`
	Attachments      []Attachment          `json:"attachments,omitempty"`
	Conference       *ConferenceData       `json:"conference,omitempty"`
	GuestPermissions *GuestPermissions     `json:"guest_permissions,omitempty"`
	Google           *GoogleEventExtension `json:"google,omitempty"`
	Created          *time.Time            `json:"created,omitempty"`
	Updated          *time.Time            `json:"updated,omitempty"`
	ReadOnly         bool                  `json:"read_only,omitempty"`
}

type EventCreateV2 struct {
	ICalUID          string                `json:"ical_uid,omitempty"`
	Title            string                `json:"title,omitempty"`
	Description      string                `json:"description,omitempty"`
	Location         string                `json:"location,omitempty"`
	Start            EventTime             `json:"start"`
	End              EventTime             `json:"end"`
	Recurrence       []string              `json:"recurrence,omitempty"`
	Attendees        []AttendeeV2          `json:"attendees,omitempty"`
	Reminders        *ReminderSettings     `json:"reminders,omitempty"`
	Visibility       string                `json:"visibility,omitempty"`
	Transparency     string                `json:"transparency,omitempty"`
	ColorID          string                `json:"color_id,omitempty"`
	Attachments      []Attachment          `json:"attachments,omitempty"`
	Conference       *ConferenceData       `json:"conference,omitempty"`
	GuestPermissions *GuestPermissions     `json:"guest_permissions,omitempty"`
	Google           *GoogleEventExtension `json:"google,omitempty"`
	SyncMarker       *SyncMarker           `json:"sync_marker,omitempty"`
}

type SyncMarker struct {
	RuleID        string `json:"rule_id"`
	SourceEventID string `json:"source_event_id"`
}

type RecurrenceView string

const (
	RecurrenceSeries   RecurrenceView = "series"
	RecurrenceExpanded RecurrenceView = "expanded"
	RecurrenceBoth     RecurrenceView = "both"
)

type MutationScope string

const (
	ScopeSeries    MutationScope = "series"
	ScopeSingle    MutationScope = "single"
	ScopeFollowing MutationScope = "following"
)

type NotificationPolicy string

const (
	NotificationsNone         NotificationPolicy = "none"
	NotificationsExternalOnly NotificationPolicy = "external_only"
	NotificationsAll          NotificationPolicy = "all"
)

type EventRef struct {
	CalendarID string `json:"calendar_id"`
	EventID    string `json:"event_id"`
}

type ListEventsRequestV2 struct {
	CalendarID  string         `json:"calendar_id,omitempty"`
	Start       time.Time      `json:"-"`
	End         time.Time      `json:"-"`
	View        RecurrenceView `json:"view,omitempty"`
	ShowDeleted bool           `json:"show_deleted,omitempty"`
	PageToken   string         `json:"page_token,omitempty"`
	MaxResults  int64          `json:"max_results,omitempty"`
}

type CreateEventRequestV2 struct {
	CalendarID    string             `json:"calendar_id"`
	Event         EventCreateV2      `json:"event"`
	Notifications NotificationPolicy `json:"notification_policy,omitempty"`
}

type UpdateEventRequestV2 struct {
	Ref           EventRef           `json:"ref"`
	Patch         EventPatchV2       `json:"patch"`
	Scope         MutationScope      `json:"scope"`
	EffectiveFrom *EventTime         `json:"effective_from,omitempty"`
	ExpectedETag  string             `json:"expected_etag,omitempty"`
	OperationID   string             `json:"operation_id,omitempty"`
	PreviewOnly   bool               `json:"preview_only,omitempty"`
	Notifications NotificationPolicy `json:"notification_policy,omitempty"`
}

type DeleteEventRequestV2 struct {
	Ref           EventRef           `json:"ref"`
	Scope         MutationScope      `json:"scope"`
	EffectiveFrom *EventTime         `json:"effective_from,omitempty"`
	ExpectedETag  string             `json:"expected_etag,omitempty"`
	OperationID   string             `json:"operation_id,omitempty"`
	PreviewOnly   bool               `json:"preview_only,omitempty"`
	Notifications NotificationPolicy `json:"notification_policy,omitempty"`
}

type InstancesRequestV2 struct {
	Ref         EventRef  `json:"ref"`
	Start       time.Time `json:"-"`
	End         time.Time `json:"-"`
	ShowDeleted bool      `json:"show_deleted,omitempty"`
	PageToken   string    `json:"page_token,omitempty"`
	MaxResults  int64     `json:"max_results,omitempty"`
}

type SearchEventsRequestV2 struct {
	CalendarID  string    `json:"calendar_id,omitempty"`
	Query       string    `json:"query"`
	Start       time.Time `json:"-"`
	End         time.Time `json:"-"`
	EventTypes  []string  `json:"event_types,omitempty"`
	ShowDeleted bool      `json:"show_deleted,omitempty"`
	PageToken   string    `json:"page_token,omitempty"`
	MaxResults  int64     `json:"max_results,omitempty"`
}

type RespondToEventRequestV2 struct {
	Ref           EventRef           `json:"ref"`
	Response      string             `json:"response"`
	Comment       string             `json:"comment,omitempty"`
	ExpectedETag  string             `json:"expected_etag,omitempty"`
	Notifications NotificationPolicy `json:"notification_policy,omitempty"`
}

type MoveEventRequestV2 struct {
	Ref                   EventRef           `json:"ref"`
	DestinationCalendarID string             `json:"destination_calendar_id"`
	ExpectedETag          string             `json:"expected_etag,omitempty"`
	Notifications         NotificationPolicy `json:"notification_policy,omitempty"`
}

type ImportEventRequestV2 struct {
	CalendarID string        `json:"calendar_id"`
	Event      EventCreateV2 `json:"event"`
}

type OperationStep struct {
	Name      string `json:"name"`
	Completed bool   `json:"completed"`
	Detail    string `json:"detail,omitempty"`
}

type RecoveryAction struct {
	Action string     `json:"action"`
	Refs   []EventRef `json:"refs,omitempty"`
}

type OperationResult struct {
	Status        string          `json:"status"`
	Event         *EventV2        `json:"event,omitempty"`
	RelatedEvents []EventV2       `json:"related_events,omitempty"`
	Steps         []OperationStep `json:"steps,omitempty"`
	Recovery      *RecoveryAction `json:"recovery,omitempty"`
	Warnings      []string        `json:"warnings,omitempty"`
}

type SourceStatus struct {
	Provider   string    `json:"provider"`
	CalendarID string    `json:"calendar_id,omitempty"`
	Complete   bool      `json:"complete"`
	Error      *APIError `json:"error,omitempty"`
}

type Page[T any] struct {
	Items         []T            `json:"items"`
	NextPageToken string         `json:"next_page_token,omitempty"`
	Complete      bool           `json:"complete"`
	Sources       []SourceStatus `json:"sources,omitempty"`
}
