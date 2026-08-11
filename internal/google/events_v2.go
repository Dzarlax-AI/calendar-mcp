package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"calendar-mcp/internal/calendar"
)

const defaultGooglePageSize int64 = 250

func (p *Provider) Capabilities(ctx context.Context, calendarID string) (calendar.CalendarCapabilities, error) {
	entry, err := p.svc.CalendarList.Get(calendarID).Context(ctx).Do()
	if err != nil {
		return calendar.CalendarCapabilities{}, err
	}
	readOnly := entry.AccessRole == "reader" || entry.AccessRole == "freeBusyReader"
	canWrite := !readOnly
	eventTypes := []string{"default"}
	if entry.Primary || calendarID == "primary" {
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
			GuestPermissions: true, ExtendedProps: true, SpecialEventTypes: entry.Primary || calendarID == "primary", OptimisticLocking: true,
		},
		MutationScopes:       []calendar.MutationScope{calendar.ScopeSeries, calendar.ScopeSingle, calendar.ScopeFollowing},
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone, calendar.NotificationsExternalOnly, calendar.NotificationsAll},
		EventTypes:           eventTypes,
	}, nil
}

func (p *Provider) ListEventsV2(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if request.View == calendar.RecurrenceBoth {
		return p.listBothViews(ctx, request)
	}
	return p.listOneView(ctx, request, request.View, request.PageToken, googlePageSize(request.MaxResults))
}

func (p *Provider) listOneView(ctx context.Context, request calendar.ListEventsRequestV2, view calendar.RecurrenceView, pageToken string, pageSize int64) (calendar.Page[calendar.EventV2], error) {
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
	response, err := call.Context(ctx).Do()
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	items := make([]calendar.EventV2, 0, len(response.Items))
	for _, event := range response.Items {
		items = append(items, fromGoogleEventV2(event, request.CalendarID, response.TimeZone))
	}
	return calendar.Page[calendar.EventV2]{Items: items, NextPageToken: response.NextPageToken, Complete: true}, nil
}

type bothPageState struct {
	SeriesToken   string `json:"series_token,omitempty"`
	ExpandedToken string `json:"expanded_token,omitempty"`
	SeriesDone    bool   `json:"series_done,omitempty"`
	ExpandedDone  bool   `json:"expanded_done,omitempty"`
}

func (p *Provider) listBothViews(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	state, err := decodeBothPageToken(request.PageToken)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	pageSize := googlePageSize(request.MaxResults)
	if pageSize > 1 {
		pageSize /= 2
	}
	var items []calendar.EventV2
	if !state.SeriesDone {
		page, err := p.listOneView(ctx, request, calendar.RecurrenceSeries, state.SeriesToken, pageSize)
		if err != nil {
			return calendar.Page[calendar.EventV2]{}, err
		}
		items = append(items, page.Items...)
		state.SeriesToken = page.NextPageToken
		state.SeriesDone = page.NextPageToken == ""
	}
	if !state.ExpandedDone {
		page, err := p.listOneView(ctx, request, calendar.RecurrenceExpanded, state.ExpandedToken, pageSize)
		if err != nil {
			return calendar.Page[calendar.EventV2]{}, err
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
	nextToken := ""
	if !state.SeriesDone || !state.ExpandedDone {
		nextToken, err = encodeBothPageToken(state)
		if err != nil {
			return calendar.Page[calendar.EventV2]{}, err
		}
	}
	return calendar.Page[calendar.EventV2]{Items: items, NextPageToken: nextToken, Complete: true}, nil
}

func (p *Provider) GetEventV2(ctx context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	event, err := p.svc.Events.Get(ref.CalendarID, ref.EventID).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	fallbackZone := ""
	if event.Start == nil || event.Start.TimeZone == "" {
		if cal, err := p.svc.Calendars.Get(ref.CalendarID).Context(ctx).Do(); err == nil {
			fallbackZone = cal.TimeZone
		}
	}
	result := fromGoogleEventV2(event, ref.CalendarID, fallbackZone)
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
