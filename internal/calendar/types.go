package calendar

import "time"

type Calendar struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Color    string `json:"color,omitempty"`
	Primary  bool   `json:"primary,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type Event struct {
	ID          string    `json:"id"`
	CalendarID  string    `json:"calendar_id"`
	Provider    string    `json:"provider"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Location    string    `json:"location,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day,omitempty"`
	Status      string    `json:"status,omitempty"`
}

type EventCreate struct {
	Title       string    `json:"title"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Description string    `json:"description,omitempty"`
	Location    string    `json:"location,omitempty"`
}

type EventUpdate struct {
	Title       *string    `json:"title,omitempty"`
	Start       *time.Time `json:"start,omitempty"`
	End         *time.Time `json:"end,omitempty"`
	Description *string    `json:"description,omitempty"`
	Location    *string    `json:"location,omitempty"`
}
