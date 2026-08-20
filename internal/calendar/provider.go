package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

func SyncMarkerValue(ruleID, sourceEventID string) string {
	sum := sha256.Sum256([]byte(ruleID + "\x00" + sourceEventID))
	return hex.EncodeToString(sum[:])
}

type Provider interface {
	Name() string
	ListCalendars(ctx context.Context) ([]Calendar, error)
	GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]Event, error)
	CreateEvent(ctx context.Context, calendarID string, event EventCreate) (*Event, error)
	UpdateEvent(ctx context.Context, calendarID string, eventID string, event EventUpdate) (*Event, error)
	DeleteEvent(ctx context.Context, calendarID string, eventID string) error
}

// AccountRoutedProvider lets one registry host multiple authenticated
// accounts for the same provider without changing the provider's public type.
type AccountRoutedProvider interface {
	Provider
	RouteName() string
	OwnsCalendar(calendarID string) bool
}

type RouteConfigurableProvider interface {
	AccountRoutedProvider
	SetRoute(route string, calendarIDs []string)
}

// RecurrenceWriteValidator lets the sync engine block a rule during dry run
// before any target mutation when the target cannot represent a series
// without changing its recurrence semantics.
type RecurrenceWriteValidator interface {
	ValidateRecurrenceWrite([]string, EventTime) error
}

// SyncMarkerLookupProvider lets the sync engine recover a target object after
// an ambiguous create or a local mapping-write failure without creating a
// duplicate on the next run.
type SyncMarkerLookupProvider interface {
	Provider
	FindEventBySyncMarkerV2(ctx context.Context, calendarID, ruleID, sourceEventID string) (*EventV2, error)
}

func ProviderRouteName(provider Provider) string {
	if routed, ok := provider.(AccountRoutedProvider); ok && routed.RouteName() != "" {
		return routed.RouteName()
	}
	return provider.Name()
}

func AccountCalendarID(providerName, connectionID, providerCalendarID string) string {
	return providerName + ":" + connectionID + ":" + providerCalendarID
}

func CanonicalCalendarID(provider Provider, providerCalendarID string) string {
	route := ProviderRouteName(provider)
	providerName, connectionID, routed := strings.Cut(route, "@")
	if routed && providerName != "" && connectionID != "" {
		return AccountCalendarID(providerName, connectionID, providerCalendarID)
	}
	return provider.Name() + ":" + providerCalendarID
}

func ProviderOwnsCalendar(provider Provider, calendarID string) bool {
	if routed, ok := provider.(AccountRoutedProvider); ok {
		return routed.OwnsCalendar(calendarID)
	}
	return true
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
