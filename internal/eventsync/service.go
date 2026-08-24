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
	defaultPollInterval           = 5 * time.Minute
	defaultRetryBase              = time.Minute
	defaultRetryMax               = 15 * time.Minute
	defaultMaxPages               = 1_000
	defaultMaxResets              = 2
	defaultMaxObjectRepairsPerRun = 1
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
	ListDueEventSyncQuarantine(context.Context, storage.CalendarSyncState, time.Time, int) ([]storage.CalendarSyncQuarantine, error)
	ApplyEventSyncObjectRepair(context.Context, storage.CalendarSyncState, storage.EventSyncRepairBatch, time.Time) error
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
	if err := s.repairOne(ctx, state, provider, providerCalendarID, policy); err != nil {
		return err
	}
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
			return s.fail(ctx, state, policy, class, retryAfter, providerFailureResult(callErr))
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
			next := s.now().Add(policy.PollInterval)
			batch.NextCursor = string(page.NextCursor)
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

// providerFailureResult preserves the adapter's safe typed classification and
// bounded provider detail for diagnostics while retaining the package sentinel
// used by callers and existing tests. Opaque provider payloads remain hidden
// by EventSyncError.Error.
func providerFailureResult(err error) error {
	var syncErr *calendar.EventSyncError
	if errors.As(err, &syncErr) && syncErr != nil {
		return errors.Join(ErrProviderFailure, syncErr)
	}
	return ErrProviderFailure
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
	if policy.MaxObjectRepairsPerRun <= 0 {
		policy.MaxObjectRepairsPerRun = defaultMaxObjectRepairsPerRun
	}
	return policy
}

func (s *Service) repairOne(ctx context.Context, state storage.CalendarSyncState, provider calendar.Provider, providerCalendarID string, policy calendar.EventSyncPolicy) error {
	repairer, ok := calendar.EventSyncObjectRepairCapability(provider)
	if !ok {
		return nil
	}
	due, err := s.Store.ListDueEventSyncQuarantine(ctx, state, s.now(), policy.MaxObjectRepairsPerRun)
	if err != nil {
		if errors.Is(err, storage.ErrCalendarSyncLeaseLost) {
			return err
		}
		return s.fail(ctx, state, policy, calendar.EventSyncTransient, 0, ErrStorageFailure)
	}
	if len(due) == 0 {
		return nil
	}
	for _, item := range due {
		if err := s.repairObject(ctx, state, providerCalendarID, policy, repairer, item); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) repairObject(ctx context.Context, state storage.CalendarSyncState, providerCalendarID string, policy calendar.EventSyncPolicy, repairer calendar.EventSyncObjectRepairProvider, item storage.CalendarSyncQuarantine) error {
	result, repairErr := repairer.RepairEventSyncObject(ctx, calendar.EventSyncObjectRepairRequest{
		CalendarID: providerCalendarID,
		Window:     calendar.EventSyncWindow{Start: state.WindowStart, End: state.WindowEnd},
		Object:     calendar.SyncObject{ObjectID: item.ObjectID, ETag: item.ETag}, Generation: state.Generation,
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if repairErr != nil {
		class, retryAfter := classify(repairErr)
		if class == calendar.EventSyncAuth || class == calendar.EventSyncPermission || class == calendar.EventSyncUnsupported {
			return s.fail(ctx, state, policy, class, retryAfter, providerFailureResult(repairErr))
		}
		// A transient/rate-limited/protocol object repair is local to this
		// object. Persist its retry and continue the ordinary feed below.
		next := s.now().Add(retryDelay(policy, retryAfter))
		warning := storage.EventSyncWarning{ObjectID: item.ObjectID, ETag: item.ETag, ErrorCode: string(calendar.EventSyncProtocol)}
		applyErr := s.Store.ApplyEventSyncObjectRepair(ctx, state, storage.EventSyncRepairBatch{ObjectID: item.ObjectID, ETag: item.ETag, Outcome: calendar.EventSyncObjectStillQuarantined, Warning: &warning, NextRepairAt: &next}, s.now())
		if errors.Is(applyErr, storage.ErrCalendarSyncLeaseLost) {
			return applyErr
		}
		if applyErr != nil {
			return s.fail(ctx, state, policy, calendar.EventSyncTransient, 0, ErrStorageFailure)
		}
		return nil
	}
	batch, err := repairBatch(result, state.CalendarID, item)
	if err != nil {
		return s.fail(ctx, state, policy, calendar.EventSyncProtocol, 0, ErrInvalidPage)
	}
	if batch.Outcome == calendar.EventSyncObjectStillQuarantined {
		next := s.now().Add(retryDelay(policy, 0))
		batch.NextRepairAt = &next
	}
	if err := s.Store.ApplyEventSyncObjectRepair(ctx, state, batch, s.now()); err != nil {
		if errors.Is(err, storage.ErrCalendarSyncLeaseLost) {
			return err
		}
		return s.fail(ctx, state, policy, calendar.EventSyncTransient, 0, ErrStorageFailure)
	}
	return nil
}

func repairBatch(result calendar.EventSyncObjectRepairResult, calendarID string, item storage.CalendarSyncQuarantine) (storage.EventSyncRepairBatch, error) {
	objectID := result.Object.ObjectID
	if objectID == "" {
		objectID = item.ObjectID
	}
	if objectID != item.ObjectID {
		return storage.EventSyncRepairBatch{}, ErrInvalidPage
	}
	switch result.Outcome {
	case calendar.EventSyncObjectReplaceMembership,
		calendar.EventSyncObjectAbsentFromProjection,
		calendar.EventSyncObjectProviderDeleted,
		calendar.EventSyncObjectStillQuarantined:
	default:
		return storage.EventSyncRepairBatch{}, ErrInvalidPage
	}
	batch := storage.EventSyncRepairBatch{ObjectID: objectID, ETag: result.Object.ETag, Outcome: result.Outcome}
	if batch.ETag == "" {
		batch.ETag = item.ETag
	}
	for _, upsert := range result.Upserts {
		if upsert.Event.ID == "" || (upsert.Object.ObjectID != "" && upsert.Object.ObjectID != objectID) {
			return storage.EventSyncRepairBatch{}, ErrInvalidPage
		}
		upsert.Event.CalendarID = calendarID
		batch.Upserts = append(batch.Upserts, storage.CachedEventUpsert{SourceObjectID: objectID, Event: upsert.Event})
	}
	if result.Outcome == calendar.EventSyncObjectStillQuarantined {
		if result.Warning == nil || result.Warning.ObjectID != objectID || result.Warning.Code != calendar.EventSyncProtocol {
			return storage.EventSyncRepairBatch{}, ErrInvalidPage
		}
		batch.Warning = &storage.EventSyncWarning{ObjectID: objectID, ETag: result.Warning.ETag, ErrorCode: string(result.Warning.Code), Diagnostic: result.Warning.Diagnostic}
		if batch.Warning.ETag == "" {
			batch.Warning.ETag = batch.ETag
		}
	}
	return batch, nil
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
		delay := retryDelay(policy, retryAfter)
		// The first transient failure may retry quickly, but a repeated failure
		// must not create a tight worker loop. Without a persisted attempt counter,
		// the previous error class is the safe durable signal for moving to the
		// provider poll interval.
		if retryAfter <= 0 && state.LastErrorCode == string(class) {
			delay = minDuration(maxDuration(delay, policy.PollInterval), policy.RetryMax)
		}
		err = s.Store.FailCalendarSync(ctx, state, string(class), now, now.Add(delay))
	}
	if errors.Is(err, storage.ErrCalendarSyncLeaseLost) {
		return err
	}
	if err != nil {
		return ErrStorageFailure
	}
	return result
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
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
	for _, warning := range page.Warnings {
		batch.Warnings = append(batch.Warnings, storage.EventSyncWarning{ObjectID: warning.ObjectID, ETag: warning.ETag, ErrorCode: string(warning.Code), Diagnostic: warning.Diagnostic})
	}
	batch.Objects = make([]storage.SyncObject, 0, len(objects))
	for _, object := range objects {
		batch.Objects = append(batch.Objects, object)
	}
	return batch
}
