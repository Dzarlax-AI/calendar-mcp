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
	UpsertMapping(context.Context, storage.Mapping) error
	DeleteMapping(context.Context, string) error
}

type Result struct{ Created, Updated, Deleted, Skipped, Warnings int }

type RecurrenceCompatibilityError struct{ Cause error }

func (e *RecurrenceCompatibilityError) Error() string {
	return "target calendar cannot preserve source recurrence: " + e.Cause.Error()
}
func (e *RecurrenceCompatibilityError) Unwrap() error { return e.Cause }

type Engine struct {
	registry *calendar.Registry
	mappings MappingStore
	now      func() time.Time
}

func New(registry *calendar.Registry, mappings MappingStore) *Engine {
	return &Engine{registry: registry, mappings: mappings, now: func() time.Time { return time.Now().UTC() }}
}

func (e *Engine) Run(ctx context.Context, rule storage.Rule, dryRun bool) (Result, error) {
	if err := storage.ValidateRule(rule); err != nil {
		return Result{}, err
	}
	sourceProvider, sourceCalendarID, err := e.registry.Resolve(rule.SourceCalendarID)
	if err != nil {
		return Result{}, err
	}
	targetProvider, targetCalendarID, err := e.registry.Resolve(rule.TargetCalendarID)
	if err != nil {
		return Result{}, err
	}
	source, ok := sourceProvider.(calendar.EventProviderV2)
	if !ok {
		return Result{}, errors.New("source provider does not support typed event listing")
	}
	target, ok := targetProvider.(calendar.EventProviderV2)
	if !ok {
		return Result{}, errors.New("target provider does not support typed event writes")
	}
	now := e.now()
	start, end := now.AddDate(0, 0, -rule.LookbackDays), now.AddDate(0, 0, rule.LookaheadDays)
	events, err := listAll(ctx, source, calendar.ListEventsRequestV2{CalendarID: sourceCalendarID, Start: start, End: end, View: calendar.RecurrenceBoth, ShowDeleted: true})
	if err != nil {
		return Result{}, fmt.Errorf("list source events: %w", err)
	}
	existing, err := e.mappings.ListMappings(ctx, rule.ID)
	if err != nil {
		return Result{}, err
	}
	byKey := map[string]storage.Mapping{}
	for _, mapping := range existing {
		byKey[mappingKey(mapping.SourceEventID, mapping.OriginalStart)] = mapping
	}
	events = deduplicateAndOrder(events)
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
	occurrences := indexOccurrences(events)
	seen := map[string]bool{}
	result := Result{}
	for _, event := range events {
		if isPlainOccurrence(event) {
			result.Skipped++
			continue
		}
		originalStart := eventOriginalStart(event)
		key := mappingKey(event.ID, originalStart)
		seen[key] = true
		mapping, mapped := byKey[key]
		hash := hashEvent(event)
		if isSeriesException(event) {
			seriesMapping, seriesMapped := findSeriesMapping(byKey, event.RecurringEventID)
			if !seriesMapped {
				return result, fmt.Errorf("source series %q has no target mapping for instance %q", event.RecurringEventID, event.ID)
			}
			if dryRun {
				if event.Status == "cancelled" {
					result.Deleted++
				} else {
					result.Updated++
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
				result.Deleted++
				if !dryRun {
					if _, err := target.DeleteEventV2(ctx, calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: targetEventID}, Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone}); err != nil {
						return result, fmt.Errorf("delete cancelled target occurrence: %w", err)
					}
					if mapped {
						if err := e.mappings.DeleteMapping(ctx, mapping.ID); err != nil {
							return result, err
						}
						delete(byKey, key)
					}
				}
				continue
			}
			if !found {
				return result, fmt.Errorf("target series %q has no occurrence matching source original start", seriesMapping.TargetEventID)
			}
			if mapped && mapping.ContentHash == hash {
				result.Skipped++
				if !dryRun {
					mapping.LastSeenAt = now
					if err := e.mappings.UpsertMapping(ctx, mapping); err != nil {
						return result, err
					}
				}
				continue
			}
			result.Updated++
			if dryRun {
				continue
			}
			if _, err := target.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: targetEventID}, Patch: mirrorPatch(event), Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone}); err != nil {
				return result, fmt.Errorf("update target occurrence exception: %w", err)
			}
			mapping = storage.Mapping{ID: newMappingID(rule.ID, event.ID, originalStart), RuleID: rule.ID, ObjectKind: objectKind(event), SourceEventID: event.ID, SourceSeriesID: event.RecurringEventID, OriginalStart: originalStart, TargetEventID: targetEventID, TargetSeriesID: seriesMapping.TargetEventID, ContentHash: hash, LastSeenAt: now, ReconciliationState: "current"}
			if err := e.mappings.UpsertMapping(ctx, mapping); err != nil {
				return result, err
			}
			byKey[key] = mapping
			continue
		}
		if event.Status == "cancelled" {
			if mapped {
				result.Deleted++
				if !dryRun {
					if _, err := target.DeleteEventV2(ctx, calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: mapping.TargetEventID}, Scope: scopeFor(event), Notifications: calendar.NotificationsNone}); err != nil {
						return result, fmt.Errorf("delete cancelled target event: %w", err)
					}
					if err := e.mappings.DeleteMapping(ctx, mapping.ID); err != nil {
						return result, err
					}
				}
			} else {
				result.Skipped++
			}
			continue
		}
		if !mapped {
			result.Created++
			if dryRun {
				if objectKind(event) == "series" {
					byKey[key] = storage.Mapping{ObjectKind: "series", SourceEventID: event.ID}
				}
				continue
			}
			lookup, ok := targetProvider.(calendar.SyncMarkerLookupProvider)
			if !ok {
				return result, fmt.Errorf("target provider %q does not support idempotent sync-marker recovery", targetProvider.Name())
			}
			created, err := lookup.FindEventBySyncMarkerV2(ctx, targetCalendarID, rule.ID, event.ID)
			if err != nil {
				return result, fmt.Errorf("recover target event by sync marker: %w", err)
			}
			if created == nil {
				created, err = target.CreateEventV2(ctx, calendar.CreateEventRequestV2{CalendarID: targetCalendarID, Event: mirrorCreate(rule.ID, event, targetProvider.Name()), Notifications: calendar.NotificationsNone})
				if err != nil {
					return result, fmt.Errorf("create target event: %w", err)
				}
			}
			mapping = storage.Mapping{ID: newMappingID(rule.ID, event.ID, originalStart), RuleID: rule.ID, ObjectKind: objectKind(event), SourceEventID: event.ID, SourceSeriesID: event.RecurringEventID, OriginalStart: originalStart, TargetEventID: created.ID, TargetSeriesID: created.RecurringEventID, ContentHash: hash, LastSeenAt: now, ReconciliationState: "current"}
			if mapping.ObjectKind == "series" {
				mapping.TargetSeriesID = created.ID
			}
			if err := e.mappings.UpsertMapping(ctx, mapping); err != nil {
				return result, err
			}
			byKey[key] = mapping
			continue
		}
		if mapping.ContentHash == hash {
			result.Skipped++
			if !dryRun {
				mapping.LastSeenAt = now
				if err := e.mappings.UpsertMapping(ctx, mapping); err != nil {
					return result, err
				}
			}
			continue
		}
		result.Updated++
		if dryRun {
			continue
		}
		_, err := target.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: mapping.TargetEventID}, Patch: mirrorPatch(event), Scope: scopeFor(event), Notifications: calendar.NotificationsNone})
		if err != nil {
			return result, fmt.Errorf("update target event: %w", err)
		}
		mapping.ContentHash, mapping.LastSeenAt = hash, now
		if err := e.mappings.UpsertMapping(ctx, mapping); err != nil {
			return result, err
		}
	}
	for _, mapping := range existing {
		if seen[mappingKey(mapping.SourceEventID, mapping.OriginalStart)] {
			continue
		}
		if mapping.ObjectKind == "exception" {
			if occurrence, ok := occurrences[seriesOccurrenceKey(mapping.SourceSeriesID, mapping.OriginalStart)]; ok {
				result.Updated++
				if !dryRun {
					if _, err := target.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: targetCalendarID, EventID: mapping.TargetEventID}, Patch: mirrorPatch(occurrence), Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone}); err != nil {
						return result, fmt.Errorf("restore target occurrence after removed exception: %w", err)
					}
					if err := e.mappings.DeleteMapping(ctx, mapping.ID); err != nil {
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

func deduplicateAndOrder(events []calendar.EventV2) []calendar.EventV2 {
	ordered := make([]calendar.EventV2, 0, len(events))
	seen := map[string]bool{}
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
	return event.RecurringEventID != "" && event.Status != "cancelled" && event.InstanceKind != "exception"
}

func isSeriesException(event calendar.EventV2) bool {
	return event.RecurringEventID != "" && (event.InstanceKind == "exception" || event.InstanceKind == "cancelled" || event.Status == "cancelled")
}

func listAll(ctx context.Context, provider calendar.EventProviderV2, request calendar.ListEventsRequestV2) ([]calendar.EventV2, error) {
	var result []calendar.EventV2
	for {
		page, err := provider.ListEventsV2(ctx, request)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Items...)
		if page.NextPageToken == "" {
			return result, nil
		}
		request.PageToken = page.NextPageToken
	}
}

func mirrorCreate(ruleID string, event calendar.EventV2, targetProvider string) calendar.EventCreateV2 {
	created := calendar.EventCreateV2{ICalUID: event.ICalUID, Title: event.Title, Description: event.Description, Location: event.Location, Start: event.Start, End: event.End, Recurrence: append([]string(nil), event.Recurrence...), Transparency: event.Transparency, Visibility: event.Visibility, SyncMarker: &calendar.SyncMarker{RuleID: ruleID, SourceEventID: event.ID}}
	if targetProvider == "google" {
		created.Google = &calendar.GoogleEventExtension{PrivateProperties: map[string]string{"calendar_sync_rule": ruleID, "calendar_source_event": event.ID}}
	}
	return created
}

func mirrorPatch(event calendar.EventV2) calendar.EventPatchV2 {
	patch := calendar.EventPatchV2{Title: present(event.Title), Description: present(event.Description), Location: present(event.Location), Start: present(event.Start), End: present(event.End), Transparency: present(event.Transparency), Visibility: present(event.Visibility)}
	if objectKind(event) == "series" {
		patch.Recurrence = present(append([]string(nil), event.Recurrence...))
	}
	return patch
}

func present[T any](value T) calendar.PatchField[T] {
	return calendar.PatchField[T]{Present: true, Value: value}
}
func scopeFor(event calendar.EventV2) calendar.MutationScope {
	if event.RecurringEventID != "" || event.OriginalStart != nil {
		return calendar.ScopeSingle
	}
	return calendar.ScopeSeries
}
func objectKind(event calendar.EventV2) string {
	switch event.InstanceKind {
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
func hashEvent(event calendar.EventV2) string {
	value := struct {
		Title, Description, Location, Status string
		Visibility, Transparency             string
		Start, End                           calendar.EventTime
		Recurrence                           []string
	}{event.Title, event.Description, event.Location, event.Status, event.Visibility, event.Transparency, event.Start, event.End, event.Recurrence}
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
