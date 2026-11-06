package syncengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type MappingStore interface {
	ListMappings(context.Context, string) ([]storage.Mapping, error)
	InsertPendingCreateMapping(context.Context, storage.Mapping) (string, error)
	InsertPendingMutationMapping(context.Context, storage.Mapping) (string, error)
	UpdateMappingCAS(context.Context, storage.Mapping, int64) error
	DeleteMappingCAS(context.Context, string, string, int64) error
	UpsertMapping(context.Context, storage.Mapping) error
	DeleteMapping(context.Context, string) error
}

const DetachedSeriesWarning = "Some recurring events are mirrored as individual events because their source series was unavailable."

const IncompleteParticipantsWarning = "Some participant lists may be incomplete."
const LegacyIncompleteParticipantsWarning = "Some participant lists are incomplete; existing descriptions were preserved until the full list is available."
const SourceOnlyPreviewWarning = "Destination was not checked; this preview does not authorize synchronization."

type Failure struct {
	Stage string
	Cause error
}

var errJobLeaseStage = errors.New("sync job lease stage")

func (e *Failure) Error() string        { return e.Stage + ": " + e.Cause.Error() }
func (e *Failure) Unwrap() error        { return e.Cause }
func (e *Failure) Is(target error) bool { return target == errJobLeaseStage && e.Stage == "job_lease" }

// ErrMissingSeriesMapping identifies an exception whose source series has no target mapping.
var ErrMissingSeriesMapping = errors.New("source series has no target mapping")

type Result struct {
	Created, Updated, Deleted, Skipped, Warnings, DetachedSeries int
	ParticipantsIncomplete                                       int
	Preview                                                      *storage.SyncRunPreview
}

func previewAction(preview *storage.SyncRunPreview, event calendar.EventV2, action, reason, targetStatus string) {
	if preview == nil {
		return
	}
	preview.Total++
	if len(preview.Entries) >= 1000 {
		preview.Truncated = true
		return
	}
	title := []rune(event.Title)
	if len(title) > 512 {
		title = title[:512]
	}
	start := event.Start
	if start.Date == "" && start.DateTime == "" && event.OriginalStart != nil {
		start = *event.OriginalStart
	}
	preview.Entries = append(preview.Entries, storage.SyncRunPreviewEntry{Action: action, Title: string(title), Start: start, Kind: objectKind(event), Reason: reason, TargetStatus: targetStatus})
}

type RecurrenceCompatibilityError struct{ Cause error }

func (e *RecurrenceCompatibilityError) Error() string {
	return "target calendar cannot preserve source recurrence: " + e.Cause.Error()
}
func (e *RecurrenceCompatibilityError) Unwrap() error { return e.Cause }

type Engine struct {
	registry      *calendar.Registry
	mappings      MappingStore
	now           func() time.Time
	dispatchFence DispatchFence
	onDispatch    func()
}

func (e *Engine) WithDispatchFence(f DispatchFence) *Engine { e.dispatchFence = f; return e }

// WithDispatchObserver runs immediately before each provider mutation. It is
// used to keep a durable run reservation parked after an uncertain outcome.
func (e *Engine) WithDispatchObserver(fn func()) *Engine { e.onDispatch = fn; return e }

var ErrUnsafeTargetAttendees = errors.New("target provider cannot guarantee attendee-free synchronization")

var ErrUnsafeRecurringSync = errors.New("recurring target mutations are not supported by the safe synchronization policy")

// ErrPendingCreateUnresolved means a previous create may have reached the
// provider even though marker lookup cannot see it yet. Never retry blindly.
var ErrPendingCreateUnresolved = errors.New("pending target creation requires reconciliation")

type selectionGuardedTarget struct {
	calendar.EventProviderV2
	registry           *calendar.Registry
	sourceID, targetID string
	dispatchFence      DispatchFence
	onDispatch         func()
}

func (g selectionGuardedTarget) check(ctx context.Context) error {
	for _, id := range []string{g.sourceID, g.targetID} {
		if id == "" {
			continue
		}
		if _, _, err := g.registry.ResolveEnabled(ctx, id); err != nil {
			return fmt.Errorf("sync calendar selection changed: %w", err)
		}
	}
	return CheckDispatchFence(ctx, g.dispatchFence)
}

func (g selectionGuardedTarget) CreateEventV2(ctx context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	if err := requireSafeNoAttendeeTarget(g.EventProviderV2); err != nil {
		return nil, err
	}
	if len(request.Event.Attendees) != 0 {
		return nil, ErrUnsafeTargetAttendees
	}
	request.Event.Attendees = nil
	request.Notifications = calendar.NotificationsNone
	request.RequireNoAttendees = true
	if err := g.check(ctx); err != nil {
		return nil, err
	}
	if g.onDispatch != nil {
		g.onDispatch()
	}
	return g.EventProviderV2.CreateEventV2(ctx, request)
}
func (g selectionGuardedTarget) UpdateEventV2(ctx context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if err := requireSafeNoAttendeeTarget(g.EventProviderV2); err != nil {
		return nil, err
	}
	if request.Patch.Attendees.Present {
		return nil, ErrUnsafeTargetAttendees
	}
	request.Notifications = calendar.NotificationsNone
	request.RequireNoAttendees = true
	if err := g.check(ctx); err != nil {
		return nil, err
	}
	if g.onDispatch != nil {
		g.onDispatch()
	}
	return g.EventProviderV2.UpdateEventV2(ctx, request)
}
func (g selectionGuardedTarget) DeleteEventV2(ctx context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if err := requireSafeNoAttendeeTarget(g.EventProviderV2); err != nil {
		return nil, err
	}
	request.Notifications = calendar.NotificationsNone
	request.RequireNoAttendees = true
	if err := g.check(ctx); err != nil {
		return nil, err
	}
	if g.onDispatch != nil {
		g.onDispatch()
	}
	return g.EventProviderV2.DeleteEventV2(ctx, request)
}

func requireSafeNoAttendeeTarget(provider calendar.EventProviderV2) error {
	capability, ok := provider.(calendar.SafeNoAttendeeMutationProvider)
	if !ok || !capability.SupportsNoAttendeeMutations() {
		return ErrUnsafeTargetAttendees
	}
	return nil
}

func New(registry *calendar.Registry, mappings MappingStore) *Engine {
	return &Engine{registry: registry, mappings: mappings, now: func() time.Time { return time.Now().UTC() }}
}

func (e *Engine) Run(ctx context.Context, rule storage.Rule, dryRun bool) (runResult Result, runErr error) {
	if rule.SyncMode == "cached_instances" {
		return Result{}, errors.New("cached instance rules require the cache-only engine")
	}
	return e.run(ctx, rule, dryRun, nil)
}

// run accepts a prevalidated local batch only from the private cache experiment.
// A caller-supplied batch is not provider provenance or permission to dispatch.
func (e *Engine) run(ctx context.Context, rule storage.Rule, dryRun bool, cachedEvents *[]calendar.EventV2) (runResult Result, runErr error) {
	stage := "rule_validation"
	defer func() {
		if runErr != nil {
			runErr = &Failure{Stage: stage, Cause: runErr}
		}
	}()
	if err := storage.ValidateRule(rule); err != nil {
		return Result{}, err
	}
	stage = "source_selection"
	var source calendar.EventProviderV2
	var sourceCalendarID string
	if cachedEvents == nil {
		sourceProvider, resolvedID, err := e.registry.ResolveEnabled(ctx, rule.SourceCalendarID)
		if err != nil {
			return Result{}, err
		}
		sourceCalendarID = resolvedID
		var ok bool
		source, ok = sourceProvider.(calendar.EventProviderV2)
		if !ok {
			return Result{}, errors.New("source provider does not support typed event listing")
		}
	}
	stage = "target_selection"
	targetProvider, targetCalendarID, err := e.registry.ResolveEnabled(ctx, rule.TargetCalendarID)
	if err != nil {
		return Result{}, err
	}
	stage = "capabilities"
	target, ok := targetProvider.(calendar.EventProviderV2)
	if !ok {
		return Result{}, errors.New("target provider does not support typed event writes")
	}
	if !dryRun {
		if err := requireSafeNoAttendeeTarget(target); err != nil {
			return Result{}, err
		}
	}
	guardSource := rule.SourceCalendarID
	if cachedEvents != nil {
		guardSource = ""
	}
	target = selectionGuardedTarget{EventProviderV2: target, registry: e.registry, sourceID: guardSource, targetID: rule.TargetCalendarID, dispatchFence: e.dispatchFence, onDispatch: e.onDispatch}
	now := e.now()
	start, end := now.AddDate(0, 0, -rule.LookbackDays), now.AddDate(0, 0, rule.LookaheadDays)
	stage = "source_read"
	var events []calendar.EventV2
	if cachedEvents == nil {
		events, err = listAll(ctx, source, calendar.ListEventsRequestV2{CalendarID: sourceCalendarID, Start: start, End: end, View: calendar.RecurrenceBoth, ShowDeleted: true})
		if err != nil {
			return Result{}, fmt.Errorf("list source events: %w", err)
		}
	} else {
		events = append([]calendar.EventV2(nil), (*cachedEvents)...)
	}
	stage = "mappings"
	existing, err := e.mappings.ListMappings(ctx, rule.ID)
	if err != nil {
		return Result{}, err
	}
	for _, m := range existing {
		if m.ReconciliationState == "pending_update" || m.ReconciliationState == "pending_delete" {
			return Result{}, ErrPendingTargetMutation
		}
	}
	byKey := map[string]storage.Mapping{}
	for _, mapping := range existing {
		byKey[mappingKey(mapping.SourceEventID, mapping.OriginalStart)] = mapping
	}
	events = deduplicateAndOrder(events)
	availableSeries := make(map[string]bool)
	seriesTitles := make(map[string]string)
	for _, event := range events {
		if objectKind(event) == "series" {
			seriesTitles[event.ID] = event.Title
			if event.Status != "cancelled" {
				availableSeries[event.ID] = true
			}
		}
	}
	// Once a rule mirrors a series as individual instances, keep that mode even
	// if the source master reappears. Creating a target series would duplicate
	// the existing standalone copies. SourceSeriesID persists the choice.
	detached := map[string]bool{}
	for _, mapping := range existing {
		if mapping.ObjectKind == "detached_occurrence" && mapping.SourceSeriesID != "" {
			detached[mapping.SourceSeriesID] = true
		}
	}
	for _, event := range events {
		if event.RecurringEventID != "" && (event.InstanceKind == calendar.OrphanOccurrence || !availableSeries[event.RecurringEventID]) {
			if _, mirrored := findSeriesMapping(byKey, event.RecurringEventID); !mirrored {
				detached[event.RecurringEventID] = true
			}
		}
	}
	// Never reinterpret an existing series instance mapping as a standalone
	// target: the latter is updated/deleted with top-level scope.
	for _, event := range events {
		if !detached[event.RecurringEventID] || event.RecurringEventID == "" {
			continue
		}
		if event.ID == "" || event.OriginalStart == nil {
			return Result{}, errors.New("detached source occurrence lacks stable identity")
		}
		if mapping, ok := byKey[mappingKey(event.ID, eventOriginalStart(event))]; ok && mapping.ObjectKind != "detached_occurrence" {
			return Result{}, fmt.Errorf("%w: incompatible occurrence mapping", ErrMissingSeriesMapping)
		}
		if _, mirrored := findSeriesMapping(byKey, event.RecurringEventID); mirrored {
			return Result{}, fmt.Errorf("%w: conflicting series and detached mappings", ErrMissingSeriesMapping)
		}
	}
	filtered := make([]calendar.EventV2, 0, len(events))
	activeDetached := map[string]bool{}
	for _, event := range events {
		if objectKind(event) == "series" && detached[event.ID] {
			activeDetached[event.ID] = true
			continue
		}
		if detached[event.RecurringEventID] {
			activeDetached[event.RecurringEventID] = true
			event.InstanceKind = calendar.OrphanOccurrence
			event.Recurrence = nil
		} else if event.InstanceKind == calendar.OrphanOccurrence {
			// A target series already exists: update its matching instance rather
			// than creating a standalone duplicate.
			event.InstanceKind = "exception"
		}
		filtered = append(filtered, event)
	}
	events = filtered
	stage = "safe_recurrence_preflight"
	if capability, ok := targetProvider.(calendar.SafeNoAttendeeRecurringMutationProvider); !ok || !capability.SupportsNoAttendeeRecurringMutations() {
		if err := preflightSafeRecurrence(events, byKey, existing); err != nil {
			return Result{}, err
		}
	}
	stage = "recurrence"
	if validator, ok := targetProvider.(calendar.RecurrenceWriteValidator); ok {
		for _, event := range events {
			if objectKind(event) != "series" {
				continue
			}
			if err := validator.ValidateRecurrenceWrite(event.Recurrence, event.Start); err != nil {
				return Result{}, &RecurrenceCompatibilityError{Cause: err}
			}
		}
	} else {
		for _, event := range events {
			if objectKind(event) == "series" {
				return Result{}, &RecurrenceCompatibilityError{Cause: errors.New("target provider cannot validate lossless recurrence")}
			}
		}
	}
	stage = "reconciliation"
	occurrences := indexOccurrences(events)
	seen := map[string]bool{}
	result := Result{Warnings: len(activeDetached), DetachedSeries: len(activeDetached)}
	for _, event := range events {
		if event.Status != "cancelled" && !event.SourceAttendeesComplete {
			result.Warnings++
			result.ParticipantsIncomplete++
		}
	}
	if dryRun {
		result.Preview = &storage.SyncRunPreview{Entries: []storage.SyncRunPreviewEntry{}}
	}
	for _, event := range events {
		previewEvent := event
		if previewEvent.Title == "" && seriesTitles[previewEvent.RecurringEventID] != "" {
			previewEvent.Title = "Series: " + seriesTitles[previewEvent.RecurringEventID]
		}
		if isPlainOccurrence(event) {
			result.Skipped++
			previewAction(result.Preview, previewEvent, "skip", "part_of_series", "not_checked")
			continue
		}
		originalStart := eventOriginalStart(event)
		key := mappingKey(event.ID, originalStart)
		seen[key] = true
		mapping, mapped := byKey[key]
		pendingCreate := mapped && mapping.ReconciliationState == "pending_create"
		if mapped && mapping.ReconciliationState == "pending_create" {
			if dryRun {
				return result, ErrPendingCreateUnresolved
			}
			if event.Status == "cancelled" {
				if _, _, err := cacheTargetConditions(mapping); err != nil {
					return result, err
				}
				lookup, ok := targetProvider.(calendar.SyncMarkerLookupProvider)
				if !ok {
					return result, errors.New("target provider cannot recover a pending standalone instance")
				}
				copy, err := lookup.FindEventBySyncMarkerV2(ctx, targetCalendarID, rule.ID, event.ID)
				if err != nil {
					return result, fmt.Errorf("recover cancelled standalone instance: %w", err)
				}
				if copy != nil {
					result.Deleted++
					previewAction(result.Preview, previewEvent, "delete", "cancelled_recovered_copy", "found")
					pending, err := e.beginMappingMutation(ctx, mapping, "pending_delete", copy.ID)
					if err != nil {
						return result, err
					}
					outcome, err := target.DeleteEventV2(ctx, calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: copy.ID}, Scope: scopeFor(event), Notifications: calendar.NotificationsNone})
					if err != nil {
						return result, err
					}
					if err = e.completeMappingDelete(ctx, pending, outcome); err != nil {
						return result, err
					}
				} else {
					return result, ErrPendingCreateUnresolved
				}
				delete(byKey, key)
				continue
			}
			// The placeholder identifies local intent, never a provider object.
			mapped = false
		}
		hash := hashEvent(event)
		if isSeriesException(event) {
			seriesMapping, seriesMapped := findSeriesMapping(byKey, event.RecurringEventID)
			if !seriesMapped {
				return result, fmt.Errorf("%w: source series %q, instance %q", ErrMissingSeriesMapping, event.RecurringEventID, event.ID)
			}
			if dryRun {
				if event.Status == "cancelled" {
					result.Deleted++
					previewAction(result.Preview, previewEvent, "delete", "cancelled_series_instance", "not_checked")
				} else {
					result.Updated++
					previewAction(result.Preview, previewEvent, "update", "series_exception", "not_checked")
				}
				continue
			}
			targetEventID, found, err := findTargetInstance(ctx, targetProvider, targetCalendarID, seriesMapping.TargetEventID, event.OriginalStart, start, end)
			if err != nil {
				return result, err
			}
			if event.Status == "cancelled" {
				if !found {
					result.Skipped++
					continue
				}
				if !mapped {
					mapping = storage.Mapping{ID: newMappingID(rule.ID, event.ID, originalStart), RuleID: rule.ID, ObjectKind: objectKind(event), SourceEventID: event.ID, SourceSeriesID: event.RecurringEventID, OriginalStart: originalStart, TargetEventID: targetEventID, TargetSeriesID: seriesMapping.TargetEventID, LastSeenAt: now}
				}
				result.Deleted++
				if !dryRun {
					pending, err := e.beginMappingMutation(ctx, mapping, "pending_delete", targetEventID)
					if err != nil {
						return result, err
					}
					outcome, err := target.DeleteEventV2(ctx, calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: targetEventID}, Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone})
					if err != nil {
						return result, fmt.Errorf("delete cancelled target occurrence: %w", err)
					}
					if err = e.completeMappingDelete(ctx, pending, outcome); err != nil {
						return result, err
					}
					delete(byKey, key)
				}
				continue
			}
			if !found {
				return result, fmt.Errorf("target series %q has no occurrence matching source original start", seriesMapping.TargetEventID)
			}
			if mapped && mapping.ContentHash == hash {
				result.Skipped++
				continue
			}
			result.Updated++
			if dryRun {
				continue
			}
			if !mapped {
				mapping = storage.Mapping{ID: newMappingID(rule.ID, event.ID, originalStart), RuleID: rule.ID, ObjectKind: objectKind(event), SourceEventID: event.ID, SourceSeriesID: event.RecurringEventID, OriginalStart: originalStart, TargetEventID: targetEventID, TargetSeriesID: seriesMapping.TargetEventID, LastSeenAt: now}
			}
			pending, err := e.beginMappingMutation(ctx, mapping, "pending_update", targetEventID)
			if err != nil {
				return result, err
			}
			outcome, err := target.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: targetEventID}, Patch: mirrorPatch(event), Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone})
			if err != nil {
				return result, fmt.Errorf("update target occurrence exception: %w", err)
			}
			mapping = storage.Mapping{ID: pending.ID, RuleID: rule.ID, ObjectKind: objectKind(event), SourceEventID: event.ID, SourceSeriesID: event.RecurringEventID, OriginalStart: originalStart, TargetEventID: targetEventID, TargetSeriesID: seriesMapping.TargetEventID, ContentHash: hash, LastSeenAt: now, ReconciliationState: "current", Incarnation: pending.Incarnation}
			if err := e.completeMappingMutation(ctx, pending, mapping, outcome); err != nil {
				return result, err
			}
			mapping.Version = pending.Version + 1
			byKey[key] = mapping
			continue
		}
		if event.Status == "cancelled" {
			if mapped {
				result.Deleted++
				previewAction(result.Preview, previewEvent, "delete", "cancelled_mapped_event", "not_checked")
				if !dryRun {
					etag, marker, err := cacheTargetConditions(mapping)
					if err != nil {
						return result, err
					}
					pending, err := e.beginMappingMutation(ctx, mapping, "pending_delete", mapping.TargetEventID)
					if err != nil {
						return result, err
					}
					outcome, err := target.DeleteEventV2(ctx, calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: mapping.TargetEventID}, Scope: scopeFor(event), Notifications: calendar.NotificationsNone, ExpectedETag: etag, ExpectedSyncMarker: marker})
					if err != nil {
						return result, fmt.Errorf("delete cancelled target event: %w", err)
					}
					if err := e.completeMappingDelete(ctx, pending, outcome); err != nil {
						return result, err
					}
				}
			} else {
				result.Skipped++
				previewAction(result.Preview, previewEvent, "skip", "cancelled_without_mapping", "not_checked")
			}
			continue
		}
		if !mapped {
			if dryRun {
				result.Created++
				previewAction(result.Preview, previewEvent, "create", "no_mapping", "not_checked")
				if objectKind(event) == "series" {
					byKey[key] = storage.Mapping{ObjectKind: "series", SourceEventID: event.ID}
				}
				continue
			}
			lookup, ok := targetProvider.(calendar.SyncMarkerLookupProvider)
			if !ok {
				return result, fmt.Errorf("target provider %q does not support idempotent sync-marker recovery", targetProvider.Name())
			}
			if !pendingCreate {
				if err := CheckDispatchFence(ctx, e.dispatchFence); err != nil {
					return result, err
				}
				// Keep the intent even if recovery or its mapping write fails:
				// later negative lookups do not prove the marker is absent.
				intentID := newMappingID(rule.ID, event.ID, originalStart)
				intent := storage.Mapping{ID: intentID, RuleID: rule.ID, ObjectKind: objectKind(event), SourceEventID: event.ID, SourceSeriesID: event.RecurringEventID, OriginalStart: originalStart, TargetEventID: "pending:" + intentID, LastSeenAt: now, ReconciliationState: "pending_create"}
				incarnation, err := e.mappings.InsertPendingCreateMapping(ctx, intent)
				if err != nil {
					return result, fmt.Errorf("persist target creation intent: %w", err)
				}
				if incarnation == "" {
					return result, ErrPendingCreateUnresolved
				}
				intent.Version = 1
				intent.Incarnation = incarnation
				mapping = intent
			}
			created, err := lookup.FindEventBySyncMarkerV2(ctx, targetCalendarID, rule.ID, event.ID)
			if err != nil {
				return result, fmt.Errorf("recover target event by sync marker: %w", err)
			}
			if created != nil && (created.ID == "" || created.Status == "cancelled" || created.InstanceKind == "cancelled") {
				return result, ErrPendingCreateUnresolved
			}
			if rule.SyncMode == "cached_instances" && created != nil && !verifiedCacheTarget(created, "", rule.ID, event.ID) {
				return result, ErrCacheTargetUnverified
			}
			if created == nil {
				if pendingCreate {
					return result, ErrPendingCreateUnresolved
				}
				result.Created++
				created, err = target.CreateEventV2(ctx, calendar.CreateEventRequestV2{CalendarID: targetCalendarID, Event: mirrorCreate(rule.ID, event, targetProvider.Name()), Notifications: calendar.NotificationsNone})
				if err != nil {
					return result, fmt.Errorf("create target event: %w", err)
				}
				if created == nil || created.ID == "" || created.Status == "cancelled" || created.InstanceKind == "cancelled" {
					return result, ErrPendingCreateUnresolved
				}
				if rule.SyncMode == "cached_instances" && !verifiedCacheTarget(created, "", rule.ID, event.ID) {
					return result, ErrCacheTargetUnverified
				}
			} else if hashEvent(*created) != hash {
				// Any recovered copy may predate a source edit. Update it through
				// the no-attendee guard before recording the new content hash.
				result.Updated++
				etag, marker, err := cacheTargetConditions(mapping)
				if err != nil {
					return result, err
				}
				pending, err := e.beginMappingMutation(ctx, mapping, "pending_update", created.ID)
				if err != nil {
					return result, err
				}
				mapping = pending
				outcome, err := target.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: created.ID}, Patch: mirrorPatch(event), Scope: scopeFor(event), Notifications: calendar.NotificationsNone, ExpectedETag: etag, ExpectedSyncMarker: marker})
				if err != nil {
					return result, fmt.Errorf("update recovered standalone instance: %w", err)
				}
				if outcome == nil || outcome.Status != "completed" {
					return result, ErrPendingTargetMutation
				}
				if mapping.ObjectKind == "cache_instance" {
					if !verifiedCacheTarget(outcome.Event, created.ID, rule.ID, event.ID) {
						return result, ErrCacheTargetUnverified
					}
					created = outcome.Event
				}
			}
			pendingVersion := mapping.Version
			mapping = storage.Mapping{ID: mapping.ID, RuleID: rule.ID, ObjectKind: objectKind(event), SourceEventID: event.ID, SourceSeriesID: event.RecurringEventID, OriginalStart: originalStart, TargetEventID: created.ID, TargetSeriesID: created.RecurringEventID, ContentHash: hash, LastSeenAt: now, ReconciliationState: "current", Version: pendingVersion, Incarnation: mapping.Incarnation}
			if mapping.ObjectKind == "cache_instance" {
				mapping.TargetETag = created.ETag
			}
			if mapping.ObjectKind == "series" {
				mapping.TargetSeriesID = created.ID
			}
			if err := e.mappings.UpdateMappingCAS(ctx, mapping, pendingVersion); err != nil {
				return result, err
			}
			mapping.Version++
			byKey[key] = mapping
			continue
		}
		if mapping.ContentHash == hash {
			result.Skipped++
			previewAction(result.Preview, previewEvent, "skip", "unchanged_mapping", "not_checked")
			continue
		}
		result.Updated++
		if dryRun {
			previewAction(result.Preview, previewEvent, "update", "changed_mapping", "not_checked")
			continue
		}
		etag, marker, err := cacheTargetConditions(mapping)
		if err != nil {
			return result, err
		}
		pending, err := e.beginMappingMutation(ctx, mapping, "pending_update", mapping.TargetEventID)
		if err != nil {
			return result, err
		}
		outcome, err := target.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: mapping.TargetEventID}, Patch: mirrorPatch(event), Scope: scopeFor(event), Notifications: calendar.NotificationsNone, ExpectedETag: etag, ExpectedSyncMarker: marker})
		if err != nil {
			return result, fmt.Errorf("update target event: %w", err)
		}
		mapping.ContentHash, mapping.LastSeenAt = hash, now
		if err := e.completeMappingMutation(ctx, pending, mapping, outcome); err != nil {
			return result, err
		}
	}
	for _, mapping := range existing {
		if seen[mappingKey(mapping.SourceEventID, mapping.OriginalStart)] {
			continue
		}
		if mapping.ObjectKind == "exception" {
			if occurrence, ok := occurrences[seriesOccurrenceKey(mapping.SourceSeriesID, mapping.OriginalStart)]; ok {
				pendingHash := incompleteRestoreHash(occurrence)
				if !occurrence.SourceAttendeesComplete && mapping.ContentHash == pendingHash {
					result.Skipped++
					continue
				}
				result.Updated++
				previewOccurrence := occurrence
				if previewOccurrence.Title == "" && seriesTitles[previewOccurrence.RecurringEventID] != "" {
					previewOccurrence.Title = "Series: " + seriesTitles[previewOccurrence.RecurringEventID]
				}
				previewAction(result.Preview, previewOccurrence, "update", "restore_series_occurrence", "not_checked")
				if !dryRun {
					pending, err := e.beginMappingMutation(ctx, mapping, "pending_update", mapping.TargetEventID)
					if err != nil {
						return result, err
					}
					outcome, err := target.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: mapping.TargetEventID}, Patch: mirrorPatch(occurrence), Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone})
					if err != nil {
						return result, fmt.Errorf("restore target occurrence after removed exception: %w", err)
					}
					if !occurrence.SourceAttendeesComplete {
						mapping.ContentHash, mapping.LastSeenAt = pendingHash, now
						if err := e.completeMappingMutation(ctx, pending, mapping, outcome); err != nil {
							return result, err
						}
					} else if err := e.completeMappingDelete(ctx, pending, outcome); err != nil {
						return result, err
					}
				}
				continue
			}
		}
		// Absence from a bounded source window is not a deletion signal. Keep
		// the mapping until a provider tombstone, explicit cancellation, or a
		// separately requested full reconciliation proves deletion.
		result.Warnings++
	}
	return result, nil
}

// preflightSafeRecurrence rejects the whole run before target writes when its
// direct reconciliation would mutate a series or a series instance. Detached
// occurrences remain eligible because they are standalone target objects.
func preflightSafeRecurrence(events []calendar.EventV2, byKey map[string]storage.Mapping, existing []storage.Mapping) error {
	seen := make(map[string]bool, len(events))
	occurrences := indexOccurrences(events)
	for _, event := range events {
		if isPlainOccurrence(event) {
			continue
		}
		key := mappingKey(event.ID, eventOriginalStart(event))
		seen[key] = true
		mapping, mapped := byKey[key]
		if mapped && mapping.ReconciliationState == "pending_create" && event.Status != "cancelled" {
			mapped = false
		}
		writes := (!mapped && event.Status != "cancelled") || (mapped && (event.Status == "cancelled" || mapping.ContentHash != hashEvent(event)))
		if isSeriesException(event) {
			writes = true
		}
		if writes && (objectKind(event) == "series" || isSeriesException(event) || mapped && (mapping.ObjectKind == "series" || mapping.ObjectKind == "exception")) {
			return ErrUnsafeRecurringSync
		}
	}
	for _, mapping := range existing {
		if seen[mappingKey(mapping.SourceEventID, mapping.OriginalStart)] || mapping.ObjectKind != "exception" {
			continue
		}
		if occurrence, found := occurrences[seriesOccurrenceKey(mapping.SourceSeriesID, mapping.OriginalStart)]; found {
			if !occurrence.SourceAttendeesComplete && mapping.ContentHash == incompleteRestoreHash(occurrence) {
				continue
			}
			return ErrUnsafeRecurringSync
		}
	}
	return nil
}

func deduplicateAndOrder(events []calendar.EventV2) []calendar.EventV2 {
	ordered := make([]calendar.EventV2, 0, len(events))
	seen := map[string]bool{}
	incomplete := make(map[string]bool)
	for _, event := range events {
		if !event.SourceAttendeesComplete {
			incomplete[mappingKey(event.ID, eventOriginalStart(event))] = true
		}
	}
	for priority := 0; priority < 3; priority++ {
		for _, event := range events {
			if eventPriority(event) != priority {
				continue
			}
			key := mappingKey(event.ID, eventOriginalStart(event))
			if seen[key] {
				continue
			}
			seen[key] = true
			if incomplete[key] {
				event.SourceAttendeesComplete = false
			}
			ordered = append(ordered, event)
		}
	}
	return ordered
}

func eventPriority(event calendar.EventV2) int {
	if objectKind(event) == "series" {
		return 0
	}
	if isSeriesException(event) {
		return 1
	}
	return 2
}

func indexOccurrences(events []calendar.EventV2) map[string]calendar.EventV2 {
	result := map[string]calendar.EventV2{}
	for _, event := range events {
		if isPlainOccurrence(event) {
			result[seriesOccurrenceKey(event.RecurringEventID, eventOriginalStart(event))] = event
		}
	}
	return result
}

func findSeriesMapping(mappings map[string]storage.Mapping, sourceSeriesID string) (storage.Mapping, bool) {
	for _, mapping := range mappings {
		if mapping.ObjectKind == "series" && mapping.SourceEventID == sourceSeriesID {
			return mapping, true
		}
	}
	return storage.Mapping{}, false
}

func findTargetInstance(ctx context.Context, provider calendar.Provider, calendarID, seriesID string, original *calendar.EventTime, start, end time.Time) (string, bool, error) {
	if original == nil {
		return "", false, errors.New("recurring instance is missing original_start")
	}
	originalInstant, err := original.Instant()
	if err != nil {
		return "", false, fmt.Errorf("invalid recurring instance original_start: %w", err)
	}
	if originalInstant.Before(start) {
		start = originalInstant
	}
	originalEnd := originalInstant.Add(time.Second)
	if original.IsAllDay() {
		originalEnd = originalInstant.AddDate(0, 0, 1)
	}
	if originalEnd.After(end) {
		end = originalEnd
	}
	instances, ok := provider.(calendar.InstanceProviderV2)
	if !ok {
		return "", false, errors.New("target provider does not support recurring instance lookup")
	}
	request := calendar.InstancesRequestV2{Ref: calendar.EventRef{CalendarID: calendarID, EventID: seriesID}, Start: start, End: end, ShowDeleted: true, MaxResults: 2500}
	for {
		page, err := instances.GetEventInstancesV2(ctx, request)
		if err != nil {
			return "", false, fmt.Errorf("list target series instances: %w", err)
		}
		for _, item := range page.Items {
			candidate := item.OriginalStart
			if candidate == nil {
				candidate = &item.Start
			}
			if sameEventTime(*original, *candidate) {
				if item.Status == "cancelled" {
					return "", false, nil
				}
				return item.ID, true, nil
			}
		}
		if page.NextPageToken == "" {
			return "", false, nil
		}
		request.PageToken = page.NextPageToken
	}
}

func sameEventTime(left, right calendar.EventTime) bool {
	if left.IsAllDay() || right.IsAllDay() {
		return left.Date != "" && left.Date == right.Date
	}
	leftInstant, leftErr := left.Instant()
	rightInstant, rightErr := right.Instant()
	return leftErr == nil && rightErr == nil && leftInstant.Equal(rightInstant)
}

func seriesOccurrenceKey(seriesID, originalStart string) string {
	return mappingKey(seriesID, originalStart)
}

func isPlainOccurrence(event calendar.EventV2) bool {
	return event.RecurringEventID != "" && event.Status != "cancelled" && event.InstanceKind != "exception" && event.InstanceKind != calendar.OrphanOccurrence
}

func isSeriesException(event calendar.EventV2) bool {
	return event.InstanceKind != calendar.OrphanOccurrence && event.RecurringEventID != "" && (event.InstanceKind == "exception" || event.InstanceKind == "cancelled" || event.Status == "cancelled")
}

func listAll(ctx context.Context, provider calendar.EventProviderV2, request calendar.ListEventsRequestV2) ([]calendar.EventV2, error) {
	var result []calendar.EventV2
	for {
		page, err := provider.ListEventsV2(ctx, request)
		if err != nil {
			return nil, err
		}
		if !page.Complete {
			return nil, errors.New("source event listing is incomplete")
		}
		result = append(result, page.Items...)
		if page.NextPageToken == "" {
			return result, nil
		}
		request.PageToken = page.NextPageToken
	}
}

func mirrorCreate(ruleID string, event calendar.EventV2, targetProvider string) calendar.EventCreateV2 {
	created := calendar.EventCreateV2{ICalUID: event.ICalUID, Title: event.Title, Description: mirrorDescription(event), Location: event.Location, Start: event.Start, End: event.End, Recurrence: append([]string(nil), event.Recurrence...), Transparency: event.Transparency, Visibility: event.Visibility, SyncMarker: &calendar.SyncMarker{RuleID: ruleID, SourceEventID: event.ID}}
	if event.InstanceKind == calendar.OrphanOccurrence {
		// A recurrence UID is shared by all source instances. Detached copies
		// need distinct, stable UIDs, particularly for UID-addressed CalDAV.
		created.ICalUID = newMappingID(ruleID, event.ID, eventOriginalStart(event)) + "@calendar-mcp"
		created.Recurrence = nil
	}
	if targetProvider == "google" {
		if event.InstanceKind == "cache_instance" {
			// Google retains deleted UIDs. Let it assign a fresh UID while
			// the stable private marker remains the cache-copy identity.
			created.ICalUID = ""
		}
		created.Google = &calendar.GoogleEventExtension{PrivateProperties: map[string]string{"calendar_sync_rule": ruleID, "calendar_source_event": event.ID}}
	}
	return created
}

func mirrorPatch(event calendar.EventV2) calendar.EventPatchV2 {
	patch := calendar.EventPatchV2{Title: present(event.Title), Location: present(event.Location), Start: present(event.Start), End: present(event.End), Transparency: present(event.Transparency), Visibility: present(event.Visibility)}
	// Unknown source participants must not erase previously mirrored context.
	if event.SourceAttendeesComplete {
		patch.Description = present(mirrorDescription(event))
	}
	if objectKind(event) == "series" {
		patch.Recurrence = present(append([]string(nil), event.Recurrence...))
	}
	return patch
}

func present[T any](value T) calendar.PatchField[T] {
	return calendar.PatchField[T]{Present: true, Value: value}
}
func scopeFor(event calendar.EventV2) calendar.MutationScope {
	if event.InstanceKind == calendar.OrphanOccurrence {
		return calendar.ScopeSeries
	}
	if event.RecurringEventID != "" || event.OriginalStart != nil {
		return calendar.ScopeSingle
	}
	return calendar.ScopeSeries
}
func objectKind(event calendar.EventV2) string {
	switch event.InstanceKind {
	case "cache_instance":
		return "cache_instance"
	case calendar.OrphanOccurrence:
		return "detached_occurrence"
	case "seriesMaster":
		return "series"
	case "exception":
		return "exception"
	case "cancelled":
		return "cancelled_occurrence"
	}
	if event.RecurringEventID != "" {
		if event.Status == "cancelled" {
			return "cancelled_occurrence"
		}
		return "occurrence"
	}
	if len(event.Recurrence) > 0 {
		return "series"
	}
	return "event"
}
func eventOriginalStart(event calendar.EventV2) string {
	if event.OriginalStart == nil {
		return ""
	}
	data, _ := json.Marshal(event.OriginalStart)
	return string(data)
}
func mappingKey(id, original string) string { return id + "\x00" + original }
func newMappingID(ruleID, eventID, original string) string {
	sum := sha256.Sum256([]byte(mappingKey(ruleID+"\x00"+eventID, original)))
	return hex.EncodeToString(sum[:])
}
func incompleteRestoreHash(event calendar.EventV2) string {
	return "pending-incomplete-restore:" + hashEvent(event)
}

func hashEvent(event calendar.EventV2) string {
	description := ""
	if event.SourceAttendeesComplete {
		description = mirrorDescription(event)
	}
	value := struct {
		Title, Description, Location, Status string
		Visibility, Transparency             string
		Start, End                           calendar.EventTime
		Recurrence                           []string
	}{event.Title, description, event.Location, event.Status, event.Visibility, event.Transparency, event.Start, event.End, event.Recurrence}
	data, _ := json.Marshal(value)
	if !event.SourceAttendeesComplete {
		data = append([]byte("incomplete-participants:"), data...)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
