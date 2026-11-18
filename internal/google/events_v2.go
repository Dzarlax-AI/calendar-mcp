package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"google.golang.org/api/googleapi"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/token"
)

const defaultGooglePageSize int64 = 250

func (p *Provider) Capabilities(ctx context.Context, calendarID string) (calendar.CalendarCapabilities, error) {
	ctx, observe, err := p.startCalendarRoleObservation(ctx, calendarID)
	if err != nil {
		return calendar.CalendarCapabilities{}, err
	}
	entry, err := p.svc.CalendarList.Get(calendarID).Context(ctx).Do()
	if err != nil {
		return calendar.CalendarCapabilities{}, authorityError(ctx, err)
	}
	knownRole := entry.AccessRole == "owner" || entry.AccessRole == "writer" || entry.AccessRole == "writerWithoutPrivateAccess" || entry.AccessRole == "reader" || entry.AccessRole == "freeBusyReader"
	if err := observeRole(ctx, observe, entry.AccessRole); err != nil {
		if !errors.Is(err, storage.ErrAuthorityClosed) || !knownRole {
			return calendar.CalendarCapabilities{}, err
		}
		if ctx.Err() != nil {
			return calendar.CalendarCapabilities{}, ctx.Err()
		}
	}
	return calendarCapabilities(entry.AccessRole, entry.Primary || calendarID == "primary"), nil
}

func calendarCapabilities(role string, primary bool) calendar.CalendarCapabilities {
	canWrite := role == "owner" || role == "writer"
	readOnly := !canWrite
	eventTypes := []string{"default"}
	if primary {
		eventTypes = append(eventTypes, "birthday", "focusTime", "outOfOffice", "workingLocation", "fromGmail")
	}
	return calendar.CalendarCapabilities{
		ReadOnly: readOnly,
		Operations: calendar.OperationCapabilities{
			List: true, Get: true, Search: true, Create: canWrite, Update: canWrite, Delete: canWrite,
			Instances: true, Respond: canWrite, Move: canWrite, Import: canWrite,
		},
		Fields: calendar.FieldCapabilities{
			Recurrence: true, Reminders: true, Attachments: true, Conferencing: true, Colors: true,
			GuestPermissions: true, ExtendedProps: true, SpecialEventTypes: primary, OptimisticLocking: true,
		},
		MutationScopes:       []calendar.MutationScope{calendar.ScopeSeries, calendar.ScopeSingle, calendar.ScopeFollowing},
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone, calendar.NotificationsExternalOnly, calendar.NotificationsAll},
		EventTypes:           eventTypes,
	}
}

func (p *Provider) ValidateRecurrenceWrite(lines []string, _ calendar.EventTime) error {
	return calendar.ValidateRecurrence(lines)
}

func (p *Provider) FindEventBySyncMarkerV2(ctx context.Context, calendarID, ruleID, sourceEventID string) (*calendar.EventV2, error) {
	if p == nil || p.svc == nil || calendarID == "" || ruleID == "" || sourceEventID == "" {
		return nil, errors.New("sync marker lookup requires calendar and marker identifiers")
	}
	pageToken := ""
	seen := map[string]bool{}
	var found *calendar.EventV2
	for page := 0; page < 250; page++ {
		call := p.svc.Events.List(calendarID).
			SingleEvents(false).
			ShowDeleted(false).
			PrivateExtendedProperty("calendar_source_event=" + sourceEventID).
			MaxResults(2)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		callCtx, observe, err := p.startRoleObservation(ctx, calendarID)
		if err != nil {
			return nil, err
		}
		response, err := call.Context(callCtx).Do()
		if err != nil {
			if binding := token.RequestBindingFromContext(callCtx); binding != nil && binding.AuthorityError != nil {
				return nil, binding.AuthorityError
			}
			if callCtx.Err() != nil {
				return nil, callCtx.Err()
			}
			var apiErr *googleapi.Error
			if errors.As(err, &apiErr) {
				return nil, &googleapi.Error{Code: apiErr.Code}
			}
			return nil, errors.New("sync marker lookup failed")
		}
		if err := observeRole(callCtx, observe, response.AccessRole); err != nil {
			return nil, err
		}
		if len(response.Items) > 2 {
			return nil, errors.New("sync marker lookup exceeded page size")
		}
		for _, item := range response.Items {
			if item == nil || item.Id == "" || item.ExtendedProperties == nil || item.ExtendedProperties.Private == nil {
				return nil, errors.New("sync marker lookup returned an unverifiable event")
			}
			properties := item.ExtendedProperties.Private
			if properties["calendar_sync_rule"] == "" || properties["calendar_source_event"] == "" {
				return nil, errors.New("sync marker lookup returned an unverifiable event")
			}
			// Verify both properties locally; a source marker alone is not
			// sufficient to establish the requested rule association.
			if properties["calendar_sync_rule"] != ruleID || properties["calendar_source_event"] != sourceEventID {
				continue
			}
			if found != nil {
				return nil, fmt.Errorf("multiple target events share the same sync marker")
			}
			event := fromGoogleEventV2(item, calendarID, response.TimeZone)
			found = &event
		}
		if response.NextPageToken == "" {
			return found, nil
		}
		if len(response.NextPageToken) > 4096 || seen[response.NextPageToken] {
			return nil, errors.New("sync marker lookup pagination is invalid")
		}
		seen[response.NextPageToken] = true
		pageToken = response.NextPageToken
	}
	return nil, errors.New("sync marker lookup exceeded page limit")
}

func (p *Provider) ListEventsV2(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if request.View == calendar.RecurrenceBoth {
		return p.listBothViews(ctx, request, nil)
	}
	page, _, err := p.listOneView(ctx, request, request.View, request.PageToken, googlePageSize(request.MaxResults))
	return page, err
}

func (p *Provider) listOneView(ctx context.Context, request calendar.ListEventsRequestV2, view calendar.RecurrenceView, pageToken string, pageSize int64) (calendar.Page[calendar.EventV2], string, error) {
	call := p.svc.Events.List(request.CalendarID).
		TimeMin(request.Start.Format(time.RFC3339)).
		TimeMax(request.End.Format(time.RFC3339)).
		ShowDeleted(request.ShowDeleted).
		MaxResults(pageSize)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	if view == calendar.RecurrenceExpanded {
		call = call.SingleEvents(true).OrderBy("startTime")
	} else {
		call = call.SingleEvents(false)
	}
	ctx, observe, err := p.startRoleObservation(ctx, request.CalendarID)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, "", err
	}
	response, err := call.Context(ctx).Do()
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, "", authorityError(ctx, err)
	}
	if err := observeRole(ctx, observe, response.AccessRole); err != nil {
		return calendar.Page[calendar.EventV2]{}, "", err
	}
	items := make([]calendar.EventV2, 0, len(response.Items))
	for _, event := range response.Items {
		items = append(items, fromGoogleEventV2(event, request.CalendarID, response.TimeZone))
	}
	return calendar.Page[calendar.EventV2]{Items: items, NextPageToken: response.NextPageToken, Complete: true}, response.TimeZone, nil
}

type bothPageState struct {
	SeriesToken   string `json:"series_token,omitempty"`
	ExpandedToken string `json:"expanded_token,omitempty"`
	SeriesDone    bool   `json:"series_done,omitempty"`
	ExpandedDone  bool   `json:"expanded_done,omitempty"`
}

type googleSeriesCache struct {
	masters     map[string]calendar.EventV2
	unavailable map[string]bool
}

func newGoogleSeriesCache() *googleSeriesCache {
	return &googleSeriesCache{masters: make(map[string]calendar.EventV2), unavailable: make(map[string]bool)}
}

// NewGatewayEventListing scopes recurring master evidence to one snapshot.
// The worker calls it once and walks the continuation sequentially.
func (p *Provider) NewGatewayEventListing() func(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	cache := newGoogleSeriesCache()
	return func(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
		return p.listBothViews(ctx, request, cache)
	}
}

func (p *Provider) listBothViews(ctx context.Context, request calendar.ListEventsRequestV2, snapshot *googleSeriesCache) (calendar.Page[calendar.EventV2], error) {
	state, err := decodeBothPageToken(request.PageToken)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	pageSize := googlePageSize(request.MaxResults)
	if pageSize > 1 {
		pageSize /= 2
	}
	var items []calendar.EventV2
	var expandedZone string
	if !state.SeriesDone {
		page, _, err := p.listOneView(ctx, request, calendar.RecurrenceSeries, state.SeriesToken, pageSize)
		if err != nil {
			return calendar.Page[calendar.EventV2]{}, err
		}
		items = append(items, page.Items...)
		state.SeriesToken = page.NextPageToken
		state.SeriesDone = page.NextPageToken == ""
	}
	// Read the series stream first during a snapshot so a later series page
	// can supply masters without one-off GETs for earlier expanded pages.
	if !state.ExpandedDone && (snapshot == nil || state.SeriesDone) {
		page, zone, err := p.listOneView(ctx, request, calendar.RecurrenceExpanded, state.ExpandedToken, pageSize)
		if err != nil {
			return calendar.Page[calendar.EventV2]{}, err
		}
		if snapshot != nil {
			expandedZone = zone
		}
		// The series view already contains every standalone event. Keep only
		// recurrence instances from the expanded view, which makes the two
		// independently paginated streams disjoint across page boundaries.
		for _, item := range page.Items {
			if item.RecurringEventID != "" {
				items = append(items, item)
			}
		}
		state.ExpandedToken = page.NextPageToken
		state.ExpandedDone = page.NextPageToken == ""
	}
	cache := snapshot
	if cache == nil {
		cache = newGoogleSeriesCache()
	}
	for _, item := range items {
		if item.InstanceKind == "seriesMaster" {
			cache.masters[item.ID] = item
		}
	}
	for i := range items {
		item := &items[i]
		if item.InstanceKind != "occurrence" || item.RecurringEventID == "" {
			continue
		}
		id := item.RecurringEventID
		master, ok := cache.masters[id]
		if !ok {
			if cache.unavailable[id] {
				item.InstanceKind = calendar.OrphanOccurrence
				continue
			}
			ref := calendar.EventRef{CalendarID: request.CalendarID, EventID: id}
			loaded, loadErr := p.getEventV2(ctx, ref, expandedZone, snapshot == nil)
			if loadErr != nil {
				var apiErr *googleapi.Error
				if errors.As(loadErr, &apiErr) && apiErr.Code == http.StatusNotFound {
					cache.unavailable[id] = true
					item.InstanceKind = calendar.OrphanOccurrence
					continue
				}
				return calendar.Page[calendar.EventV2]{}, fmt.Errorf("load Google series master %q: %w", id, loadErr)
			}
			master = *loaded
			cache.masters[id] = master
		}
		if googleInstanceDiffers(master, *item) {
			item.InstanceKind = "exception"
		}
	}
	nextToken := ""
	if !state.SeriesDone || !state.ExpandedDone {
		nextToken, err = encodeBothPageToken(state)
		if err != nil {
			return calendar.Page[calendar.EventV2]{}, err
		}
	}
	return calendar.Page[calendar.EventV2]{Items: items, NextPageToken: nextToken, Complete: true}, nil
}

func googleInstanceDiffers(master, instance calendar.EventV2) bool {
	if instance.OriginalStart == nil {
		return true
	}
	if !sameGoogleEventTime(instance.Start, *instance.OriginalStart) {
		return true
	}
	masterStart, masterStartErr := master.Start.Instant()
	masterEnd, masterEndErr := master.End.Instant()
	instanceStart, instanceStartErr := instance.Start.Instant()
	instanceEnd, instanceEndErr := instance.End.Instant()
	if masterStartErr != nil || masterEndErr != nil || instanceStartErr != nil || instanceEndErr != nil || masterEnd.Sub(masterStart) != instanceEnd.Sub(instanceStart) {
		return true
	}
	return master.Title != instance.Title || master.Description != instance.Description || master.Location != instance.Location || master.Visibility != instance.Visibility || master.Transparency != instance.Transparency
}

func sameGoogleEventTime(left, right calendar.EventTime) bool {
	if left.IsAllDay() || right.IsAllDay() {
		return left.Date != "" && left.Date == right.Date
	}
	leftInstant, leftErr := left.Instant()
	rightInstant, rightErr := right.Instant()
	return leftErr == nil && rightErr == nil && leftInstant.Equal(rightInstant)
}

func (p *Provider) GetEventV2(ctx context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	return p.getEventV2(ctx, ref, "", true)
}

func (p *Provider) getEventV2(ctx context.Context, ref calendar.EventRef, fallbackZone string, lookupZone bool) (*calendar.EventV2, error) {
	if !lookupZone {
		ctx = token.WithRequestBinding(ctx, &token.RequestBinding{})
	}
	event, err := p.svc.Events.Get(ref.CalendarID, ref.EventID).Context(ctx).Do()
	if err != nil {
		if !lookupZone {
			err = authorityError(ctx, err)
		}
		return nil, err
	}
	if lookupZone && (event.Start == nil || event.Start.TimeZone == "") {
		if cal, err := p.svc.Calendars.Get(ref.CalendarID).Context(ctx).Do(); err == nil {
			fallbackZone = cal.TimeZone
		}
	}
	result := fromGoogleEventV2(event, ref.CalendarID, fallbackZone)
	// This full GET sets no maxAttendees restriction. Google may omit the array
	// for a personal event; attendeesOmitted still blocks partial responses.
	// Keep source completeness separate and conservative for cached events.
	result.AttendeesKnown = event.Organizer != nil && event.Organizer.Self && !event.AttendeesOmitted && (event.Attendees == nil || result.SourceAttendeesComplete)
	return &result, nil
}

func (p *Provider) GetEventInstancesV2(ctx context.Context, request calendar.InstancesRequestV2) (calendar.Page[calendar.EventV2], error) {
	if request.Start.IsZero() || request.End.IsZero() || !request.End.After(request.Start) {
		return calendar.Page[calendar.EventV2]{}, fmt.Errorf("instances require a valid bounded start and end")
	}
	call := p.svc.Events.Instances(request.Ref.CalendarID, request.Ref.EventID).
		TimeMin(request.Start.Format(time.RFC3339)).
		TimeMax(request.End.Format(time.RFC3339)).
		ShowDeleted(request.ShowDeleted).
		MaxResults(googlePageSize(request.MaxResults))
	if request.PageToken != "" {
		call = call.PageToken(request.PageToken)
	}
	response, err := call.Context(ctx).Do()
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	items := make([]calendar.EventV2, 0, len(response.Items))
	for _, event := range response.Items {
		items = append(items, fromGoogleEventV2(event, request.Ref.CalendarID, response.TimeZone))
	}
	return calendar.Page[calendar.EventV2]{Items: items, NextPageToken: response.NextPageToken, Complete: true}, nil
}

func googlePageSize(value int64) int64 {
	if value <= 0 {
		return defaultGooglePageSize
	}
	if value > 2500 {
		return 2500
	}
	return value
}

func encodeBothPageToken(state bothPageState) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeBothPageToken(value string) (bothPageState, error) {
	if value == "" {
		return bothPageState{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return bothPageState{}, fmt.Errorf("invalid both-view page token: %w", err)
	}
	var state bothPageState
	if err := json.Unmarshal(data, &state); err != nil {
		return bothPageState{}, fmt.Errorf("invalid both-view page token: %w", err)
	}
	return state, nil
}

var _ calendar.EventProviderV2 = (*Provider)(nil)
var _ calendar.InstanceProviderV2 = (*Provider)(nil)
