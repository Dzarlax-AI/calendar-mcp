package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

const followingOperationProperty = "calendarMcpOperation"

const maxFollowingPages = 1000

func (s *Service) updateFollowing(ctx context.Context, provider calendar.Provider, request calendar.UpdateEventRequestV2, prefixedCalendarID string) (*calendar.OperationResult, error) {
	v2 := provider.(calendar.EventProviderV2)
	instances, ok := provider.(calendar.InstanceProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), prefixedCalendarID, "following updates require instance listing")
	}
	lookup, ok := provider.(calendar.FollowingLookupProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), prefixedCalendarID, "following updates require idempotency lookup")
	}
	parent, err := v2.GetEventV2(ctx, request.Ref)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	if len(parent.Recurrence) == 0 {
		return nil, invalidArgument("following scope requires a recurring parent series")
	}
	if err := followingRecurrenceSupported(parent.Recurrence); err != nil {
		return nil, unsupported(provider.Name(), prefixedCalendarID, err.Error())
	}
	operationID := request.OperationID
	if operationID == "" {
		operationID, err = newOperationID()
		if err != nil {
			return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
		}
	}
	existingReplacement, err := lookup.FindEventByOperationIDV2(ctx, request.Ref.CalendarID, operationID)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	if existingReplacement == nil && request.ExpectedETag != "" && parent.ETag != request.ExpectedETag {
		return nil, &calendar.APIError{Code: calendar.ErrorConflict, Message: "event ETag does not match expected_etag", Provider: provider.Name(), CalendarID: prefixedCalendarID, EventID: request.Ref.EventID}
	}
	if existingReplacement != nil && eventTimesEqual(existingReplacement.Start, *request.EffectiveFrom) {
		elapsed, countErr := countPriorInstances(ctx, instances, request.Ref, parent.Start, *request.EffectiveFrom)
		if countErr != nil {
			return nil, countErr
		}
		if recurrenceTrimmedBefore(parent.Recurrence, *request.EffectiveFrom, elapsed) {
			return completedFollowing(provider.Name(), prefixedCalendarID, parent, existingReplacement, operationID, true), nil
		}
	}
	occurrence, elapsed, err := followingOccurrence(ctx, instances, request.Ref, *request.EffectiveFrom, parent.Start)
	if err != nil {
		return nil, err
	}
	oldRecurrence, futureRecurrence, err := splitRecurrence(parent.Recurrence, *request.EffectiveFrom, elapsed)
	if err != nil {
		return nil, invalidArgument(err.Error())
	}
	future := eventCreateFromV2(*parent)
	future.Start = occurrence.Start
	future.End = occurrence.End
	future.Recurrence = futureRecurrence
	if err := mergeFollowingPatch(&future, request.Patch); err != nil {
		return nil, invalidArgument(err.Error())
	}
	if future.Google == nil {
		future.Google = &calendar.GoogleEventExtension{}
	}
	if future.Google.PrivateProperties == nil {
		future.Google.PrivateProperties = map[string]string{}
	}
	future.Google.PrivateProperties[followingOperationProperty] = operationID

	preview := &calendar.OperationResult{
		Status: "preview",
		RelatedEvents: []calendar.EventV2{
			{ID: parent.ID, CalendarID: prefixedCalendarID, Provider: provider.Name(), Recurrence: oldRecurrence, Start: parent.Start, End: parent.End},
			{CalendarID: prefixedCalendarID, Provider: provider.Name(), Recurrence: future.Recurrence, Start: future.Start, End: future.End},
		},
		Steps:    []calendar.OperationStep{{Name: "create replacement series", Detail: "notifications forced to none"}, {Name: "trim original series", Detail: "guarded by ETag"}},
		Warnings: []string{"Google resets future instance exceptions when a series is split"},
	}
	if request.PreviewOnly {
		return preview, nil
	}

	replacement := existingReplacement
	createdNow := false
	if replacement == nil {
		replacement, err = v2.CreateEventV2(ctx, calendar.CreateEventRequestV2{
			CalendarID: request.Ref.CalendarID, Event: future, Notifications: calendar.NotificationsNone,
		})
		if err != nil {
			return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
		}
		createdNow = true
	}
	if recurrenceEqual(parent.Recurrence, oldRecurrence) {
		return completedFollowing(provider.Name(), prefixedCalendarID, parent, replacement, operationID, existingReplacement != nil), nil
	}
	if existingReplacement != nil && request.ExpectedETag != "" && parent.ETag != request.ExpectedETag {
		return &calendar.OperationResult{
			Status: "partial_failure", RelatedEvents: []calendar.EventV2{*parent, *replacement},
			Recovery: &calendar.RecoveryAction{Action: "review replacement series and trim the original manually", Refs: []calendar.EventRef{{CalendarID: prefixedCalendarID, EventID: ensurePrefix(provider.Name(), parent.ID)}, {CalendarID: prefixedCalendarID, EventID: ensurePrefix(provider.Name(), replacement.ID)}}},
			Warnings: []string{"an idempotent replacement exists, but the original series changed since the requested ETag"},
		}, nil
	}
	trimResult, trimErr := v2.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{
		Ref: request.Ref, Scope: calendar.ScopeSeries, ExpectedETag: parent.ETag, Notifications: request.Notifications,
		Patch: calendar.EventPatchV2{Recurrence: calendar.PatchField[[]string]{Present: true, Value: oldRecurrence}},
	})
	if trimErr == nil {
		trimmed := parent
		if trimResult != nil && trimResult.Event != nil {
			trimmed = trimResult.Event
		}
		return completedFollowing(provider.Name(), prefixedCalendarID, trimmed, replacement, operationID, existingReplacement != nil), nil
	}
	if createdNow {
		_, compensationErr := v2.DeleteEventV2(ctx, calendar.DeleteEventRequestV2{
			Ref: calendar.EventRef{CalendarID: request.Ref.CalendarID, EventID: replacement.ID}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone,
		})
		if compensationErr == nil {
			return nil, providerFailure(provider.Name(), prefixedCalendarID, fmt.Errorf("trim original series: %w; replacement series was removed", trimErr))
		}
		return &calendar.OperationResult{
			Status: "partial_failure", RelatedEvents: []calendar.EventV2{*parent, *replacement},
			Steps:    []calendar.OperationStep{{Name: "create replacement series", Completed: true}, {Name: "trim original series", Detail: trimErr.Error()}, {Name: "remove replacement series", Detail: compensationErr.Error()}},
			Recovery: &calendar.RecoveryAction{Action: "delete the replacement series or trim the original series to remove overlap", Refs: []calendar.EventRef{{CalendarID: prefixedCalendarID, EventID: ensurePrefix(provider.Name(), parent.ID)}, {CalendarID: prefixedCalendarID, EventID: ensurePrefix(provider.Name(), replacement.ID)}}},
		}, nil
	}
	return nil, providerFailure(provider.Name(), prefixedCalendarID, fmt.Errorf("trim original series after idempotent replacement: %w", trimErr))
}

func (s *Service) deleteFollowing(ctx context.Context, provider calendar.Provider, request calendar.DeleteEventRequestV2, prefixedCalendarID string) (*calendar.OperationResult, error) {
	v2 := provider.(calendar.EventProviderV2)
	instances, ok := provider.(calendar.InstanceProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), prefixedCalendarID, "following deletes require instance listing")
	}
	parent, err := v2.GetEventV2(ctx, request.Ref)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	if len(parent.Recurrence) == 0 {
		return nil, invalidArgument("following scope requires a recurring parent series")
	}
	if request.ExpectedETag != "" && parent.ETag != request.ExpectedETag {
		return nil, &calendar.APIError{Code: calendar.ErrorConflict, Message: "event ETag does not match expected_etag", Provider: provider.Name(), CalendarID: prefixedCalendarID, EventID: request.Ref.EventID}
	}
	_, elapsed, err := followingOccurrence(ctx, instances, request.Ref, *request.EffectiveFrom, parent.Start)
	if err != nil {
		return nil, err
	}
	oldRecurrence, _, err := splitRecurrence(parent.Recurrence, *request.EffectiveFrom, elapsed)
	if err != nil {
		return nil, invalidArgument(err.Error())
	}
	if request.PreviewOnly {
		return &calendar.OperationResult{Status: "preview", RelatedEvents: []calendar.EventV2{{ID: parent.ID, CalendarID: prefixedCalendarID, Provider: provider.Name(), Recurrence: oldRecurrence, Start: parent.Start, End: parent.End}}, Steps: []calendar.OperationStep{{Name: "trim original series", Detail: "guarded by ETag"}}}, nil
	}
	result, err := v2.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{
		Ref: request.Ref, Scope: calendar.ScopeSeries, ExpectedETag: parent.ETag, Notifications: request.Notifications,
		Patch: calendar.EventPatchV2{Recurrence: calendar.PatchField[[]string]{Present: true, Value: oldRecurrence}},
	})
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	result.Status = "completed"
	result.Steps = append(result.Steps, calendar.OperationStep{Name: "trim original series", Completed: true})
	return result, nil
}

func followingOccurrence(ctx context.Context, provider calendar.InstanceProviderV2, ref calendar.EventRef, effective calendar.EventTime, parentStart calendar.EventTime) (calendar.EventV2, int, error) {
	effectiveInstant, _ := effective.Instant()
	windowEnd := effectiveInstant.Add(48 * time.Hour)
	var occurrence *calendar.EventV2
	token := ""
	seen := make(map[string]struct{})
	for pageNumber := 0; pageNumber < maxFollowingPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return calendar.EventV2{}, 0, err
		}
		page, err := provider.GetEventInstancesV2(ctx, calendar.InstancesRequestV2{Ref: ref, Start: effectiveInstant.Add(-time.Second), End: windowEnd, ShowDeleted: true, PageToken: token, MaxResults: 2500})
		if err != nil {
			return calendar.EventV2{}, 0, err
		}
		for i := range page.Items {
			candidate := page.Items[i]
			original := candidate.Start
			if candidate.OriginalStart != nil {
				original = *candidate.OriginalStart
			}
			if eventTimesEqual(original, effective) {
				occurrence = &candidate
				break
			}
		}
		if occurrence != nil || page.NextPageToken == "" {
			break
		}
		if _, duplicate := seen[page.NextPageToken]; duplicate {
			return calendar.EventV2{}, 0, fmt.Errorf("instance pagination repeated page token")
		}
		seen[page.NextPageToken] = struct{}{}
		token = page.NextPageToken
	}
	if occurrence == nil {
		return calendar.EventV2{}, 0, invalidArgument("effective_from does not identify an occurrence in the series")
	}
	elapsed, err := countPriorInstances(ctx, provider, ref, parentStart, effective)
	if err != nil {
		return calendar.EventV2{}, 0, err
	}
	return *occurrence, elapsed, nil
}

func countPriorInstances(ctx context.Context, provider calendar.InstanceProviderV2, ref calendar.EventRef, parentStart, effective calendar.EventTime) (int, error) {
	parentInstant, _ := parentStart.Instant()
	effectiveInstant, _ := effective.Instant()
	elapsed := 0
	token := ""
	seen := make(map[string]struct{})
	for pageNumber := 0; pageNumber < maxFollowingPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		prior, err := provider.GetEventInstancesV2(ctx, calendar.InstancesRequestV2{Ref: ref, Start: parentInstant.Add(-time.Second), End: effectiveInstant, ShowDeleted: true, PageToken: token, MaxResults: 2500})
		if err != nil {
			return 0, err
		}
		elapsed += len(prior.Items)
		token = prior.NextPageToken
		if token == "" {
			return elapsed, nil
		}
		if _, duplicate := seen[token]; duplicate {
			return 0, fmt.Errorf("instance pagination repeated page token")
		}
		seen[token] = struct{}{}
	}
	return 0, fmt.Errorf("instance pagination exceeded %d pages", maxFollowingPages)
}

func splitRecurrence(lines []string, effective calendar.EventTime, elapsed int) ([]string, []string, error) {
	oldLines := append([]string(nil), lines...)
	futureLines := append([]string(nil), lines...)
	found := false
	for i, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(line), "RRULE:") {
			continue
		}
		found = true
		body := line[len("RRULE:"):]
		parts := strings.Split(body, ";")
		originalCount := 0
		kept := make([]string, 0, len(parts))
		for _, part := range parts {
			upper := strings.ToUpper(part)
			if strings.HasPrefix(upper, "COUNT=") {
				originalCount, _ = strconv.Atoi(strings.TrimSpace(strings.SplitN(part, "=", 2)[1]))
				continue
			}
			if strings.HasPrefix(upper, "UNTIL=") {
				continue
			}
			kept = append(kept, part)
		}
		if elapsed <= 0 {
			return nil, nil, fmt.Errorf("effective_from must be after at least one occurrence; delete the whole series instead")
		}
		oldParts := append([]string(nil), kept...)
		if originalCount > 0 {
			if elapsed >= originalCount {
				return nil, nil, fmt.Errorf("effective_from is outside the recurrence count")
			}
			oldParts = append(oldParts, fmt.Sprintf("COUNT=%d", elapsed))
			futureLines[i] = "RRULE:" + strings.Join(append(append([]string(nil), kept...), fmt.Sprintf("COUNT=%d", originalCount-elapsed)), ";")
		} else {
			oldParts = append(oldParts, "UNTIL="+untilBefore(effective))
		}
		oldLines[i] = "RRULE:" + strings.Join(oldParts, ";")
	}
	if !found {
		return nil, nil, fmt.Errorf("following scope requires an RRULE")
	}
	return oldLines, futureLines, nil
}

func recurrenceTrimmedBefore(lines []string, effective calendar.EventTime, elapsed int) bool {
	wantUntil := untilBefore(effective)
	for _, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(line), "RRULE:") {
			continue
		}
		for _, part := range strings.Split(line[len("RRULE:"):], ";") {
			keyValue := strings.SplitN(part, "=", 2)
			if len(keyValue) != 2 {
				continue
			}
			switch strings.ToUpper(keyValue[0]) {
			case "UNTIL":
				return keyValue[1] == wantUntil
			case "COUNT":
				count, err := strconv.Atoi(keyValue[1])
				return err == nil && count == elapsed
			}
		}
	}
	return false
}

func untilBefore(value calendar.EventTime) string {
	instant, _ := value.Instant()
	if value.IsAllDay() {
		return instant.AddDate(0, 0, -1).Format("20060102")
	}
	return instant.Add(-time.Second).UTC().Format("20060102T150405Z")
}

func followingRecurrenceSupported(lines []string) error {
	for _, line := range lines {
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "RDATE") {
			return fmt.Errorf("following mutations with RDATE are not supported safely")
		}
	}
	return nil
}

func eventCreateFromV2(event calendar.EventV2) calendar.EventCreateV2 {
	return calendar.EventCreateV2{Title: event.Title, Description: event.Description, Location: event.Location, Start: event.Start, End: event.End,
		Recurrence: append([]string(nil), event.Recurrence...), Attendees: append([]calendar.AttendeeV2(nil), event.Attendees...), Reminders: event.Reminders,
		Visibility: event.Visibility, Transparency: event.Transparency, ColorID: event.ColorID, Attachments: append([]calendar.Attachment(nil), event.Attachments...),
		Conference: event.Conference, GuestPermissions: event.GuestPermissions, Google: cloneGoogleExtension(event.Google)}
}

func mergeFollowingPatch(event *calendar.EventCreateV2, patch calendar.EventPatchV2) error {
	apply := func(field calendar.PatchField[string], target *string) {
		if field.Present {
			*target = field.Value
		}
	}
	apply(patch.Title, &event.Title)
	apply(patch.Description, &event.Description)
	apply(patch.Location, &event.Location)
	apply(patch.Visibility, &event.Visibility)
	apply(patch.Transparency, &event.Transparency)
	apply(patch.ColorID, &event.ColorID)
	if patch.Start.Present {
		if patch.Start.Null {
			return fmt.Errorf("start cannot be null")
		}
		event.Start = patch.Start.Value
	}
	if patch.End.Present {
		if patch.End.Null {
			return fmt.Errorf("end cannot be null")
		}
		event.End = patch.End.Value
	}
	if patch.Recurrence.Present {
		event.Recurrence = append([]string(nil), patch.Recurrence.Value...)
	}
	if patch.Attendees.Present {
		event.Attendees = append([]calendar.AttendeeV2(nil), patch.Attendees.Value...)
	}
	if patch.Reminders.Present {
		if patch.Reminders.Null {
			event.Reminders = nil
		} else {
			value := patch.Reminders.Value
			event.Reminders = &value
		}
	}
	if patch.Attachments.Present {
		event.Attachments = append([]calendar.Attachment(nil), patch.Attachments.Value...)
	}
	if patch.Conference.Present {
		if patch.Conference.Null {
			event.Conference = nil
		} else {
			value := patch.Conference.Value
			event.Conference = &value
		}
	}
	if patch.GuestPermissions.Present {
		if patch.GuestPermissions.Null {
			event.GuestPermissions = nil
		} else {
			value := patch.GuestPermissions.Value
			event.GuestPermissions = &value
		}
	}
	if patch.Google.Present {
		if patch.Google.Null {
			event.Google = nil
		} else {
			value := patch.Google.Value
			event.Google = cloneGoogleExtension(&value)
		}
	}
	if err := calendar.ValidateEventTimeRangeV2(event.Start, event.End); err != nil {
		return err
	}
	return calendar.ValidateRecurrence(event.Recurrence)
}

func cloneGoogleExtension(value *calendar.GoogleEventExtension) *calendar.GoogleEventExtension {
	if value == nil {
		return nil
	}
	copy := *value
	copy.PrivateProperties = cloneStringMap(value.PrivateProperties)
	copy.SharedProperties = cloneStringMap(value.SharedProperties)
	return &copy
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func eventTimesEqual(left, right calendar.EventTime) bool {
	if left.IsAllDay() || right.IsAllDay() {
		return left.Date == right.Date && left.IsAllDay() == right.IsAllDay()
	}
	l, lerr := left.Instant()
	r, rerr := right.Instant()
	return lerr == nil && rerr == nil && l.Equal(r)
}

func recurrenceEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func completedFollowing(provider, calendarID string, original, replacement *calendar.EventV2, operationID string, reused bool) *calendar.OperationResult {
	normalizeEvent(original, provider, calendarID)
	normalizeEvent(replacement, provider, calendarID)
	createDetail := "created with notifications disabled"
	if reused {
		createDetail = "reused by operation marker " + operationID
	}
	return &calendar.OperationResult{Status: "completed", RelatedEvents: []calendar.EventV2{*original, *replacement}, Steps: []calendar.OperationStep{{Name: "create replacement series", Completed: true, Detail: createDetail}, {Name: "trim original series", Completed: true}}, Warnings: []string{"Google resets future instance exceptions when a series is split"}}
}

func newOperationID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return hex.EncodeToString(data), nil
}
