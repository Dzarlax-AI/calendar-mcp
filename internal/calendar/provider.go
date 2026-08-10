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

type CapabilityProvider interface {
	Provider
	Capabilities(ctx context.Context, calendarID string) (CalendarCapabilities, error)
}

type EventProviderV2 interface {
	CapabilityProvider
	ListEventsV2(ctx context.Context, request ListEventsRequestV2) (Page[EventV2], error)
	GetEventV2(ctx context.Context, ref EventRef) (*EventV2, error)
	CreateEventV2(ctx context.Context, request CreateEventRequestV2) (*EventV2, error)
	UpdateEventV2(ctx context.Context, request UpdateEventRequestV2) (*OperationResult, error)
	DeleteEventV2(ctx context.Context, request DeleteEventRequestV2) (*OperationResult, error)
}

type InstanceProviderV2 interface {
	Provider
	GetEventInstancesV2(ctx context.Context, request InstancesRequestV2) (Page[EventV2], error)
}

type SearchProviderV2 interface {
	Provider
	SearchEventsV2(ctx context.Context, request SearchEventsRequestV2) (Page[EventV2], error)
}

type ResponseProviderV2 interface {
	Provider
	RespondToEventV2(ctx context.Context, request RespondToEventRequestV2) (*OperationResult, error)
}

type MoveProviderV2 interface {
	Provider
	MoveEventV2(ctx context.Context, request MoveEventRequestV2) (*OperationResult, error)
}

type ImportProviderV2 interface {
	Provider
	ImportEventV2(ctx context.Context, request ImportEventRequestV2) (*EventV2, error)
}

// FollowingLookupProviderV2 exposes the idempotency lookup needed by the
// application-level, non-atomic this-and-following workflow.
type FollowingLookupProviderV2 interface {
	Provider
	FindEventByOperationIDV2(ctx context.Context, calendarID, operationID string) (*EventV2, error)
}
