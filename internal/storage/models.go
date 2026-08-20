package storage

import "time"

const SchemaVersion = 1

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
