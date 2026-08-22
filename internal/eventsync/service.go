// Package eventsync coordinates optional provider incremental-sync adapters
// with the durable event read model.
package eventsync

import (
	"context"
	"errors"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

const (
	defaultPollInterval = 5 * time.Minute
	defaultRetryBase    = time.Minute
	defaultRetryMax     = 15 * time.Minute
	defaultMaxPages     = 1_000
	defaultMaxResets    = 2
)

var (
	ErrCapabilityUnavailable = errors.New("calendar provider does not support event synchronization")
	ErrInvalidPage           = errors.New("invalid event sync page")
	ErrRepeatedPage          = errors.New("event sync provider repeated a page token")
	ErrPageLimit             = errors.New("event sync provider exceeded page limit")
	ErrResetLimit            = errors.New("event sync provider exceeded reset limit")
	ErrProviderFailure       = errors.New("event sync provider failure")
	ErrStorageFailure        = errors.New("event sync storage failure")
)

// CalendarResolver resolves a durable canonical calendar ID into its provider
// and provider-local ID. calendar.Registry satisfies this interface.
type CalendarResolver interface {
	Resolve(string) (calendar.Provider, string, error)
}

// CalendarSyncStore is the narrow Wave 1 storage surface required by the
// coordinator. Keeping it as an interface makes replay and atomic-final-page
// behaviour testable without requiring adapters to import storage.
type CalendarSyncStore interface {
	ApplyEventSyncPage(context.Context, storage.CalendarSyncState, storage.EventSyncBatch, bool, time.Time) error
	FailCalendarSync(context.Context, storage.CalendarSyncState, string, time.Time, time.Time) error
	ParkCalendarSync(context.Context, storage.CalendarSyncState, string, time.Time) error
	ResetCalendarSync(context.Context, storage.CalendarSyncState, time.Time) (*storage.CalendarSyncState, error)
}

// Service drains provider pages for an already-claimed calendar sync state.
// PolicyFor receives the resolved provider and can override an adapter's
// provider-specific polling/backoff policy. Without an override, optional
// adapter policy capability is used; otherwise conservative defaults apply.
type Service struct {
	Store     CalendarSyncStore
	Resolver  CalendarResolver
	PolicyFor func(calendar.Provider) calendar.EventSyncPolicy
	Now       func() time.Time
}

func NewService(store CalendarSyncStore, resolver CalendarResolver, policyFor func(calendar.Provider) calendar.EventSyncPolicy) *Service {
	return &Service{Store: store, Resolver: resolver, PolicyFor: policyFor}
}

// RunOne synchronizes a state for which storage already granted a lease. It
// never persists page tokens. Intermediate pages are committed idempotently;
// the durable cursor advances only with the terminal page's transaction.
func (s *Service) RunOne(ctx context.Context, state storage.CalendarSyncState) error {
	if s == nil || s.Store == nil || s.Resolver == nil {
		return errors.New("event sync service is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !state.WindowEnd.After(state.WindowStart) {
		return s.fail(ctx, state, s.policy(nil), calendar.EventSyncProtocol, 0, ErrInvalidPage)
	}
	provider, providerCalendarID, err := s.Resolver.Resolve(state.CalendarID)
	if err != nil {
		return s.fail(ctx, state, s.policy(nil), calendar.EventSyncProtocol, 0, ErrProviderFailure)
	}
	syncer, ok := calendar.EventSyncCapability(provider)
	if !ok {
		return s.fail(ctx, state, s.policy(provider), calendar.EventSyncUnsupported, 0, ErrCapabilityUnavailable)
	}
	policy := s.policy(provider)
	mode := calendar.EventSyncIncremental
	if state.Cursor == "" {
		mode = calendar.EventSyncReplacement
	}

	pageToken := calendar.EventSyncPageToken("")
	seenTokens := make(map[calendar.EventSyncPageToken]struct{})
	resets := 0
	degraded := false
	for pages := 0; ; pages++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pages >= policy.MaxPages {
			return s.fail(ctx, state, policy, calendar.EventSyncTransient, 0, ErrPageLimit)
		}
		page, callErr := syncer.SyncEvents(ctx, calendar.EventSyncRequest{
			CalendarID: providerCalendarID,
			Window:     calendar.EventSyncWindow{Start: state.WindowStart, End: state.WindowEnd},
			Cursor:     calendar.EventSyncCursor(state.Cursor),
			PageToken:  pageToken,
			Generation: state.Generation,
			Mode:       mode,
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		if callErr != nil {
			class, retryAfter := classify(callErr)
			return s.fail(ctx, state, policy, class, retryAfter, ErrProviderFailure)
		}
		if err := validatePage(mode, page); err != nil {
			return s.fail(ctx, state, policy, calendar.EventSyncProtocol, 0, ErrInvalidPage)
		}
		if len(page.Warnings) != 0 {
			degraded = true
		}
		if page.ResetRequired {
			resets++
			if resets > policy.MaxResets {
				return s.fail(ctx, state, policy, calendar.EventSyncTransient, 0, ErrResetLimit)
			}
			reset, resetErr := s.Store.ResetCalendarSync(ctx, state, s.now())
			if err := ctx.Err(); err != nil {
				return err
			}
			if errors.Is(resetErr, storage.ErrCalendarSyncLeaseLost) {
				return resetErr
			}
			if resetErr != nil || reset == nil {
				return s.fail(ctx, state, policy, calendar.EventSyncTransient, 0, ErrStorageFailure)
			}
			state = *reset
			mode = calendar.EventSyncReplacement
			pageToken = ""
			clear(seenTokens)
			degraded = false
			continue
		}
		if !page.Complete {
			if _, repeated := seenTokens[page.NextPageToken]; repeated {
				return s.fail(ctx, state, policy, calendar.EventSyncProtocol, 0, ErrRepeatedPage)
			}
			seenTokens[page.NextPageToken] = struct{}{}
		}

		batch := toStorageBatch(page, state.CalendarID, mode == calendar.EventSyncReplacement, degraded)
		if page.Complete {
			delay := policy.PollInterval
			if degraded {
				delay = degradedRetryDelay(policy, state)
			}
			next := s.now().Add(delay)
			if !degraded {
				batch.NextCursor = string(page.NextCursor)
			}
			batch.NextSyncAt = &next
		}
		applyErr := s.Store.ApplyEventSyncPage(ctx, state, batch, page.Complete, s.now())
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(applyErr, storage.ErrCalendarSyncLeaseLost) {
			return applyErr
		}
		if applyErr != nil {
			return s.fail(ctx, state, policy, calendar.EventSyncTransient, 0, ErrStorageFailure)
		}
		if page.Complete {
			return nil
		}
		pageToken = page.NextPageToken
	}
}

func degradedRetryDelay(policy calendar.EventSyncPolicy, state storage.CalendarSyncState) time.Duration {
	delay := retryDelay(policy, 0)
	if state.LastErrorCode == string(calendar.EventSyncProtocol) && policy.PollInterval > delay {
		return policy.PollInterval
	}
	return delay
}

func (s *Service) policy(provider calendar.Provider) calendar.EventSyncPolicy {
	policy := calendar.EventSyncPolicy{}
	if provider != nil && s.PolicyFor != nil {
		policy = s.PolicyFor(provider)
	} else if provider != nil {
		if policyProvider, ok := calendar.EventSyncPolicyCapability(provider); ok {
			policy = policyProvider.EventSyncPolicy()
		}
	}
	if policy.PollInterval <= 0 {
		policy.PollInterval = defaultPollInterval
	}
	if policy.RetryBase <= 0 {
		policy.RetryBase = defaultRetryBase
	}
	if policy.RetryMax <= 0 {
		policy.RetryMax = defaultRetryMax
	}
	if policy.RetryMax < policy.RetryBase {
		policy.RetryMax = policy.RetryBase
	}
	if policy.MaxPages <= 0 {
		policy.MaxPages = defaultMaxPages
	}
	if policy.MaxResets <= 0 {
		policy.MaxResets = defaultMaxResets
	}
	return policy
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// fail completes an owned claim as either a scheduled retry or a parked,
// nonretryable error. Lease loss and cancellation are terminal for this
// attempt and must not issue a second mutation.
func (s *Service) fail(ctx context.Context, state storage.CalendarSyncState, policy calendar.EventSyncPolicy, class calendar.EventSyncErrorClass, retryAfter time.Duration, result error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now()
	var err error
	if parks(class) {
		err = s.Store.ParkCalendarSync(ctx, state, string(class), now)
	} else {
		err = s.Store.FailCalendarSync(ctx, state, string(class), now, now.Add(retryDelay(policy, retryAfter)))
	}
	if errors.Is(err, storage.ErrCalendarSyncLeaseLost) {
		return err
	}
	if err != nil {
		return ErrStorageFailure
	}
	return result
}

func parks(class calendar.EventSyncErrorClass) bool {
	switch class {
	case calendar.EventSyncAuth, calendar.EventSyncPermission, calendar.EventSyncUnsupported, calendar.EventSyncProtocol:
		return true
	default:
		return false
	}
}

func retryDelay(policy calendar.EventSyncPolicy, retryAfter time.Duration) time.Duration {
	// A provider-supplied RetryAfter is a server instruction, not a suggestion:
	// do not shorten it with our normal polling/backoff bounds.
	if retryAfter > 0 {
		return retryAfter
	}
	delay := policy.RetryBase
	limit := policy.RetryMax
	if policy.PollInterval > 0 && policy.PollInterval < limit {
		limit = policy.PollInterval
	}
	if delay > limit {
		delay = limit
	}
	return delay
}

func classify(err error) (calendar.EventSyncErrorClass, time.Duration) {
	var syncErr *calendar.EventSyncError
	if errors.As(err, &syncErr) && syncErr != nil && syncErr.Class != "" {
		return syncErr.Class, syncErr.RetryAfter
	}
	var apiErr *calendar.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		switch apiErr.Code {
		case calendar.ErrorRateLimited:
			return calendar.EventSyncRateLimited, 0
		case calendar.ErrorPermissionDenied:
			return calendar.EventSyncPermission, 0
		case calendar.ErrorUnsupportedCapability:
			return calendar.EventSyncUnsupported, 0
		case calendar.ErrorInvalidArgument, calendar.ErrorInvalidRecurrence:
			return calendar.EventSyncProtocol, 0
		case calendar.ErrorProviderUnavailable:
			return calendar.EventSyncTransient, 0
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return calendar.EventSyncTransient, 0
	}
	return calendar.EventSyncTransient, 0
}

func validatePage(mode calendar.EventSyncMode, page calendar.EventSyncPage) error {
	for _, warning := range page.Warnings {
		if warning.Code != calendar.EventSyncProtocol || warning.ObjectID == "" {
			return ErrInvalidPage
		}
	}
	if page.ResetRequired {
		if page.Complete || page.NextPageToken != "" || page.NextCursor != "" || len(page.Upserts) != 0 || len(page.DeletedEventIDs) != 0 || len(page.DeletedObjectIDs) != 0 || len(page.ReplacedObjectIDs) != 0 || len(page.Inventory) != 0 {
			return ErrInvalidPage
		}
		return nil
	}
	if page.Complete {
		if page.NextPageToken != "" || (mode == calendar.EventSyncIncremental && page.NextCursor == "") {
			return ErrInvalidPage
		}
	} else if page.NextPageToken == "" || page.NextCursor != "" {
		return ErrInvalidPage
	}

	objectMembers := make(map[string]struct{}, len(page.Upserts))
	deletedObjects := make(map[string]struct{}, len(page.DeletedObjectIDs))
	for _, upsert := range page.Upserts {
		if upsert.Event.ID == "" {
			return ErrInvalidPage
		}
		if upsert.Object.ObjectID != "" {
			objectMembers[upsert.Object.ObjectID] = struct{}{}
		}
	}
	for _, objectID := range page.DeletedObjectIDs {
		if objectID == "" {
			return ErrInvalidPage
		}
		deletedObjects[objectID] = struct{}{}
	}
	for _, eventID := range page.DeletedEventIDs {
		if eventID == "" {
			return ErrInvalidPage
		}
	}
	for _, objectID := range page.ReplacedObjectIDs {
		if objectID == "" {
			return ErrInvalidPage
		}
		if _, deleted := deletedObjects[objectID]; deleted {
			return ErrInvalidPage
		}
		if _, complete := objectMembers[objectID]; !complete {
			return ErrInvalidPage
		}
	}
	for _, object := range page.Inventory {
		if object.ObjectID == "" {
			return ErrInvalidPage
		}
	}
	return nil
}

func toStorageBatch(page calendar.EventSyncPage, canonicalCalendarID string, fullSync, degraded bool) storage.EventSyncBatch {
	batch := storage.EventSyncBatch{
		DeletedEventIDs:   append([]string(nil), page.DeletedEventIDs...),
		DeletedObjectIDs:  append([]string(nil), page.DeletedObjectIDs...),
		ReplacedObjectIDs: append([]string(nil), page.ReplacedObjectIDs...),
		FullSync:          fullSync,
		Degraded:          degraded,
	}
	if degraded {
		batch.ErrorCode = string(calendar.EventSyncProtocol)
	}
	objects := make(map[string]storage.SyncObject, len(page.Inventory)+len(page.Upserts))
	for _, object := range page.Inventory {
		objects[object.ObjectID] = storage.SyncObject{ObjectID: object.ObjectID, ETag: object.ETag}
	}
	for _, upsert := range page.Upserts {
		event := upsert.Event
		event.CalendarID = canonicalCalendarID
		sourceObjectID := upsert.Object.ObjectID
		batch.Upserts = append(batch.Upserts, storage.CachedEventUpsert{SourceObjectID: sourceObjectID, Event: event})
		if sourceObjectID != "" {
			objects[sourceObjectID] = storage.SyncObject{ObjectID: sourceObjectID, ETag: upsert.Object.ETag}
		}
	}
	batch.Objects = make([]storage.SyncObject, 0, len(objects))
	for _, object := range objects {
		batch.Objects = append(batch.Objects, object)
	}
	return batch
}
