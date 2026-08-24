package calendar

import (
	"context"
	"fmt"
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
	// ETag is the response representation tag observed with the malformed
	// object. It lets storage reset repair backoff only when the provider has
	// actually changed that object; it must never be exposed publicly.
	ETag string
	// Diagnostic is retained only by the encrypted quarantine artifact store;
	// it must never be serialized into UI responses or errors.
	Diagnostic *EventSyncDiagnostic
}

// EventSyncDiagnostic carries bounded provider bytes across the adapter/storage
// boundary. Storage encrypts RawPayload before persistence.
type EventSyncDiagnostic struct {
	ProviderStatus int
	ContentType    string
	ProviderReason string
	RawPayload     []byte
	Truncated      bool
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
	// MaxObjectRepairsPerRun bounds repair work performed under a normal
	// calendar lease. Zero lets the coordinator use the conservative default.
	MaxObjectRepairsPerRun int
}

// EventSyncProvider is an optional capability. It intentionally does not
// extend Provider so legacy providers continue to compile and operate.
type EventSyncProvider interface {
	SyncEvents(context.Context, EventSyncRequest) (EventSyncPage, error)
}

// EventSyncObjectRepairOutcome distinguishes a provider deletion from an
// object which still exists but no longer belongs to the frozen projection.
// Storage may tombstone cached membership for both cases, but callers must not
// use a lossy boolean because their retry and diagnostics semantics differ.
type EventSyncObjectRepairOutcome string

const (
	EventSyncObjectReplaceMembership    EventSyncObjectRepairOutcome = "replace_membership"
	EventSyncObjectAbsentFromProjection EventSyncObjectRepairOutcome = "absent_from_projection"
	EventSyncObjectProviderDeleted      EventSyncObjectRepairOutcome = "provider_deleted"
	EventSyncObjectStillQuarantined     EventSyncObjectRepairOutcome = "still_quarantined"
)

// EventSyncObjectRepairRequest addresses one known provider object. CalendarID
// is provider-local and the opaque object ID/ETag remain internal only.
type EventSyncObjectRepairRequest struct {
	CalendarID string
	Window     EventSyncWindow
	Object     SyncObject
	Generation int64
}

// EventSyncObjectRepairResult contains one explicit repair outcome. A
// ReplaceMembership result must carry the complete current membership for the
// object; StillQuarantined must carry a protocol warning for the same object.
type EventSyncObjectRepairResult struct {
	Object  SyncObject
	Outcome EventSyncObjectRepairOutcome
	Upserts []EventSyncUpsert
	Warning *EventSyncWarning
}

// EventSyncObjectRepairProvider is optional and intentionally separate from
// EventSyncProvider: adapters can support normal sync before they grow a safe
// object-repair path.
type EventSyncObjectRepairProvider interface {
	RepairEventSyncObject(context.Context, EventSyncObjectRepairRequest) (EventSyncObjectRepairResult, error)
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

// EventSyncObjectRepairCapability discovers optional object repair support.
func EventSyncObjectRepairCapability(provider Provider) (EventSyncObjectRepairProvider, bool) {
	repairer, ok := provider.(EventSyncObjectRepairProvider)
	return repairer, ok
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
	Class          EventSyncErrorClass
	RetryAfter     time.Duration
	ProviderStatus int
	ProviderReason string
	Cause          error
}

func (e *EventSyncError) Error() string {
	if e == nil || e.Class == "" {
		return "event sync provider failure"
	}
	message := "event sync " + string(e.Class) + " failure"
	if e.ProviderStatus > 0 {
		message += fmt.Sprintf(" (provider_status=%d", e.ProviderStatus)
		if e.ProviderReason != "" {
			message += ", provider_reason=" + e.ProviderReason
		}
		message += ")"
	} else if e.ProviderReason != "" {
		message += " (provider_reason=" + e.ProviderReason + ")"
	}
	return message
}

func (e *EventSyncError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
