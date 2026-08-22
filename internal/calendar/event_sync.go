package calendar

import (
	"context"
	"time"
)

// EventSyncMode describes whether a provider is producing changes since a
// cursor or a complete replacement snapshot for the frozen projection window.
type EventSyncMode string

const (
	EventSyncIncremental EventSyncMode = "incremental"
	EventSyncReplacement EventSyncMode = "replacement"
)

// EventSyncWindow is the provider-authoritative projection interval. A sync
// attempt must use the same value for every page; callers treat it as an
// immutable value object.
type EventSyncWindow struct {
	Start time.Time
	End   time.Time
}

// EventSyncCursor and EventSyncPageToken are opaque provider values. They are
// deliberately distinct so an adapter cannot accidentally use a page token as
// a durable cursor. Do not include either value in an error or log message.
type EventSyncCursor string
type EventSyncPageToken string

// EventSyncRequest is one provider page request. CalendarID is provider-local;
// the coordinator owns canonical/routed calendar IDs and resolves them before
// calling an adapter.
type EventSyncRequest struct {
	CalendarID string
	Window     EventSyncWindow
	Cursor     EventSyncCursor
	PageToken  EventSyncPageToken
	Generation int64
	Mode       EventSyncMode
}

// SyncObject identifies a provider object which can materialize one or more
// events (for example, a CalDAV resource). It is provider data, not a storage
// type, so provider adapters remain independent of the read model.
type SyncObject struct {
	ObjectID string
	ETag     string
}

// EventSyncUpsert materializes one event from its owning provider object. An
// empty ObjectID is valid for providers without an object inventory; storage
// then uses the event ID as the stable object identity.
type EventSyncUpsert struct {
	Object SyncObject
	Event  EventV2
}

// EventSyncWarning identifies a provider object whose page data was accepted
// with a protocol-level degradation. ObjectID is provider data and must not be
// included in errors or logs.
type EventSyncWarning struct {
	Code     EventSyncErrorClass
	ObjectID string
}

// EventSyncPage is a provider-neutral, single-page delta. Inventory is the
// object inventory observed on this page. ReplacedObjectIDs is an explicit
// completeness assertion: all current event membership for each listed object
// is present in Upserts on this page. Empty objects use DeletedObjectIDs.
//
// A non-terminal page has NextPageToken and no NextCursor. A terminal page has
// Complete=true and no NextPageToken; incremental pages carry a final cursor,
// while replacement-only adapters may intentionally leave NextCursor empty. A
// reset page has ResetRequired=true and no mutations or continuation values.
type EventSyncPage struct {
	Upserts           []EventSyncUpsert
	DeletedEventIDs   []string
	DeletedObjectIDs  []string
	ReplacedObjectIDs []string
	Inventory         []SyncObject
	NextPageToken     EventSyncPageToken
	NextCursor        EventSyncCursor
	ResetRequired     bool
	Complete          bool
	Warnings          []EventSyncWarning
}

// EventSyncPolicy is the bounded scheduling policy chosen for a provider.
// Zero values let the coordinator use conservative defaults.
type EventSyncPolicy struct {
	PollInterval time.Duration
	RetryBase    time.Duration
	RetryMax     time.Duration
	MaxPages     int
	MaxResets    int
}

// EventSyncProvider is an optional capability. It intentionally does not
// extend Provider so legacy providers continue to compile and operate.
type EventSyncProvider interface {
	SyncEvents(context.Context, EventSyncRequest) (EventSyncPage, error)
}

// EventSyncPolicyProvider is an optional capability for adapters with
// provider-specific scheduling limits. It intentionally remains separate from
// EventSyncProvider so sync-capable legacy adapters can use coordinator
// defaults without implementing a policy method.
type EventSyncPolicyProvider interface {
	EventSyncPolicy() EventSyncPolicy
}

// EventSyncCapability discovers the optional incremental-sync capability.
func EventSyncCapability(provider Provider) (EventSyncProvider, bool) {
	syncer, ok := provider.(EventSyncProvider)
	return syncer, ok
}

// EventSyncPolicyCapability discovers optional provider scheduling defaults.
func EventSyncPolicyCapability(provider Provider) (EventSyncPolicyProvider, bool) {
	policyProvider, ok := provider.(EventSyncPolicyProvider)
	return policyProvider, ok
}

// EventSyncErrorClass is a provider-safe failure category. Its string value,
// rather than a provider response body, is persisted by the coordinator.
type EventSyncErrorClass string

const (
	EventSyncTransient   EventSyncErrorClass = "transient"
	EventSyncRateLimited EventSyncErrorClass = "rate_limited"
	EventSyncAuth        EventSyncErrorClass = "auth"
	EventSyncPermission  EventSyncErrorClass = "permission"
	EventSyncUnsupported EventSyncErrorClass = "unsupported"
	EventSyncProtocol    EventSyncErrorClass = "protocol"
)

// EventSyncError lets adapters supply a safe category and optional RetryAfter
// without leaking opaque provider cursors through an error string.
type EventSyncError struct {
	Class      EventSyncErrorClass
	RetryAfter time.Duration
	Cause      error
}

func (e *EventSyncError) Error() string {
	if e == nil || e.Class == "" {
		return "event sync provider failure"
	}
	return "event sync " + string(e.Class) + " failure"
}

func (e *EventSyncError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
