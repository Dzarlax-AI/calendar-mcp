package storage

import (
	"time"

	"calendar-mcp/internal/calendar"
)

const SchemaVersion = 6

type Connection struct {
	ID, Provider, AccountFingerprint, DisplayName, Status string
	EncryptedCredentials                                  []byte
	CredentialVersion                                     int
	ScopesJSON                                            string
	LastVerifiedAt                                        *time.Time
	LastErrorCode                                         string
	CreatedAt, UpdatedAt                                  time.Time
}

type Calendar struct {
	ID, ConnectionID, ProviderCalendarID, Name, Timezone string
	CanRead, CanWrite, SupportsRecurrence                bool
	DiscoveredAt                                         time.Time
}

type Rule struct {
	ID, SourceCalendarID, TargetCalendarID, State string
	IntervalSeconds, LookbackDays, LookaheadDays  int
	RecurrenceMode, NotificationPolicy            string
	CopyAttendees                                 bool
	NextRunAt                                     *time.Time
	ConsecutiveFailures                           int
	CreatedAt, UpdatedAt                          time.Time
}

type Job struct {
	ID, RuleID, Kind, State, ClaimedBy string
	AvailableAt, CreatedAt             time.Time
	ClaimedAt, FinishedAt              *time.Time
	Attempt                            int
}

type Run struct {
	ID, JobID, RuleID, Trigger, Outcome string
	StartedAt                           time.Time
	FinishedAt                          *time.Time
	CreatedCount, UpdatedCount          int
	DeletedCount, SkippedCount          int
	WarningCount                        int
	ErrorCode, ErrorSummary             string
	DryRun                              bool
}

type Mapping struct {
	ID, RuleID, ObjectKind, SourceEventID string
	SourceSeriesID, OriginalStart         string
	TargetEventID, TargetSeriesID         string
	ContentHash, ReconciliationState      string
	LastSeenAt                            time.Time
}

type OAuthAttempt struct {
	StateHash, Provider, ConnectionID, Mode, ReturnPath string
	EncryptedVerifier                                   []byte
	ExpiresAt                                           time.Time
	ConsumedAt                                          *time.Time
}

// SyncWindow bounds the provider-authoritative event projection.
type SyncWindow struct {
	Start time.Time
	End   time.Time
}

// CalendarSyncState is the durable cursor and worker lease for one readable
// calendar. Cursor values are opaque provider data and must not be exposed.
type CalendarSyncState struct {
	CalendarID, Strategy, Cursor, Status string
	WindowStart, WindowEnd               time.Time
	Generation                           int64
	NextSyncAt                           time.Time
	LastStartedAt, LastSuccessAt         *time.Time
	LastErrorCode, LeaseOwner            string
	LeaseUntil                           *time.Time
}

// SyncObject records provider objects (for example CalDAV resources) used by
// inventory-based synchronization strategies.
type SyncObject struct {
	ObjectID string
	ETag     string
}

// CachedEventUpsert associates a projected event with the provider object
// that owns it. An explicitly empty SourceObjectID is only for providers
// without object inventory and falls back to Event.ID.
type CachedEventUpsert struct {
	SourceObjectID string
	Event          calendar.EventV2
}

// EventSyncBatch is a provider-neutral page of projection changes. FullSync
// declares that the page belongs to a replacement snapshot; only a successful
// final page sweeps older generations.
type EventSyncBatch struct {
	Upserts          []CachedEventUpsert
	DeletedEventIDs  []string
	DeletedObjectIDs []string
	// ReplacedObjectIDs identifies objects for which this batch contains the
	// complete current membership. Empty objects must use DeletedObjectIDs.
	ReplacedObjectIDs []string
	Objects           []SyncObject
	NextCursor        string
	NextSyncAt        *time.Time
	FullSync          bool
	Degraded          bool
	ErrorCode         string
	Warnings          []EventSyncWarning
}

// EventSyncWarning is the storage-safe representation of an object-scoped
// protocol degradation. It deliberately contains no provider payload.
type EventSyncWarning struct {
	ObjectID   string
	ETag       string
	ErrorCode  string
	Diagnostic *calendar.EventSyncDiagnostic
}

// CalendarSyncQuarantine persists an unresolved, repairable provider object.
// Active rows intentionally have no expiry: only a confirmed upsert or delete
// may resolve them.
type CalendarSyncQuarantine struct {
	CalendarID, ObjectID, ETag, ErrorCode string
	FirstSeenAt, LastSeenAt, NextRepairAt time.Time
	RepairAttempts                        int
	Active                                bool
	ProviderMutationAuthorizedETag        string
}

type RawEventSyncArtifact struct {
	CalendarID, ObjectID, ETag, PayloadSHA256, ContentType, ProviderReason string
	ProviderStatus                                                         int
	RawPayload                                                             []byte
	Truncated                                                              bool
	CapturedAt, ExpiresAt                                                  time.Time
}

// EventSyncQuarantineDiagnostic is the operator-facing, bounded view of an
// active quarantined object. It intentionally contains no raw provider payload.
type EventSyncQuarantineDiagnostic struct {
	CalendarSyncQuarantine
	CalendarName, Provider, SyncStatus string
	LastSuccessAt                      *time.Time
	LastErrorCode                      string
	Artifact                           *RawEventSyncArtifactMetadata
}

// EventSyncProviderCorrection is a durable operator-audit record for a
// successful provider-side correction. It intentionally outlives the resolved
// quarantine row and does not affect source health.
type EventSyncProviderCorrection struct {
	CalendarID, ObjectID, Outcome string
	CorrectedAt                   time.Time
	CalendarName, Provider        string
}

// RawEventSyncArtifactMetadata identifies whether a non-expired diagnostic
// payload remains available without decrypting or loading it.
type RawEventSyncArtifactMetadata struct {
	ETag, PayloadSHA256, ContentType, ProviderReason string
	ProviderStatus                                   int
	Truncated                                        bool
	CapturedAt, ExpiresAt                            time.Time
}

// EventSyncRepairBatch is one already-fetched repair result. Outcome is kept
// explicit rather than reducing provider deletion and out-of-window objects to
// the same boolean.
type EventSyncRepairBatch struct {
	ObjectID     string
	ETag         string
	Outcome      calendar.EventSyncObjectRepairOutcome
	Upserts      []CachedEventUpsert
	Warning      *EventSyncWarning
	NextRepairAt *time.Time
}

// CachedSourceStatus keeps the read model's freshness outcome separate from
// the provider-facing calendar.SourceStatus contract.
type CachedSourceStatus struct {
	Provider, CalendarID, Status, ErrorCode string
	LastSuccessAt                           *time.Time
	NextSyncAt                              *time.Time
	Stale                                   bool
}
