package calendar

import (
	"context"
	"time"
)

type Calendar struct {
	// Timezone is internal discovery metadata; existing list JSON remains unchanged.
	Timezone string `json:"-"`
	// GoogleDiscoveryCredentialDigest binds metadata to the credential used for its discovery page.
	GoogleDiscoveryCredentialDigest [32]byte `json:"-"`
	// GoogleWriteAccessProven is internal evidence for strict catalogs only.
	GoogleWriteAccessProven bool   `json:"-"`
	ID                      string `json:"id"`
	Provider                string `json:"provider"`
	Name                    string `json:"name"`
	Color                   string `json:"color,omitempty"`
	Primary                 bool   `json:"primary,omitempty"`
	ReadOnly                bool   `json:"read_only,omitempty"`
}

type Attendee struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status,omitempty"` // accepted, declined, tentative, needsAction
	Optional bool   `json:"optional,omitempty"`
}

type Event struct {
	ID            string     `json:"id"`
	CalendarID    string     `json:"calendar_id"`
	Provider      string     `json:"provider"`
	Title         string     `json:"title"`
	Description   string     `json:"description,omitempty"`
	Location      string     `json:"location,omitempty"`
	Start         time.Time  `json:"start"`
	End           time.Time  `json:"end"`
	AllDay        bool       `json:"all_day,omitempty"`
	Status        string     `json:"status,omitempty"`
	Attendees     []Attendee `json:"attendees,omitempty"`
	OnlineMeeting string     `json:"online_meeting,omitempty"` // video call URL
}

type EventCreate struct {
	Title       string     `json:"title"`
	Start       time.Time  `json:"start"`
	End         time.Time  `json:"end"`
	AllDay      bool       `json:"all_day,omitempty"`
	Description string     `json:"description,omitempty"`
	Location    string     `json:"location,omitempty"`
	Attendees   []Attendee `json:"attendees,omitempty"`
	VideoCall   bool       `json:"video_call,omitempty"` // auto-create Google Meet / Teams
}

type EventUpdate struct {
	Title       *string     `json:"title,omitempty"`
	Start       *time.Time  `json:"start,omitempty"`
	End         *time.Time  `json:"end,omitempty"`
	AllDay      *bool       `json:"all_day,omitempty"`
	Description *string     `json:"description,omitempty"`
	Location    *string     `json:"location,omitempty"`
	Attendees   *[]Attendee `json:"attendees,omitempty"`
}

// DetailDiscoveryEntry retains the provider's raw access role for a complete
// staged discovery; it is not part of the legacy calendar response contract.
type DetailDiscoveryEntry struct {
	Calendar   Calendar
	AccessRole string
}

type DetailDiscoveryProvider interface {
	DiscoverDetailCalendars(ctx context.Context) ([]DetailDiscoveryEntry, error)
}
