package calendar

import (
	"context"
	"time"
)

type Provider interface {
	Name() string
	ListCalendars(ctx context.Context) ([]Calendar, error)
	GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]Event, error)
	CreateEvent(ctx context.Context, calendarID string, event EventCreate) (*Event, error)
	UpdateEvent(ctx context.Context, calendarID string, eventID string, event EventUpdate) (*Event, error)
	DeleteEvent(ctx context.Context, calendarID string, eventID string) error
}
