package application

import (
	"context"
	"fmt"
	"sync"

	"calendar-mcp/internal/calendar"
)

func (s *Service) GetEventInstances(ctx context.Context, request calendar.InstancesRequestV2) (calendar.Page[calendar.EventV2], error) {
	if err := validateRange(request.Start, request.End); err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	provider, rawRef, err := s.resolveEventRef(request.Ref)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	instances, ok := provider.(calendar.InstanceProviderV2)
	if !ok {
		return calendar.Page[calendar.EventV2]{}, unsupported(provider.Name(), request.Ref.CalendarID, "event instances are not supported")
	}
	prefixedCalendarID := request.Ref.CalendarID
	request.Ref = rawRef
	page, err := instances.GetEventInstancesV2(ctx, request)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	normalizePage(&page, provider.Name(), prefixedCalendarID)
	return page, nil
}

func (s *Service) SearchEvents(ctx context.Context, request calendar.SearchEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if request.Query == "" {
		return calendar.Page[calendar.EventV2]{}, invalidArgument("query is required")
	}
	if request.Start.IsZero() != request.End.IsZero() {
		return calendar.Page[calendar.EventV2]{}, invalidArgument("start and end must be provided together")
	}
	if !request.Start.IsZero() {
		if err := validateRange(request.Start, request.End); err != nil {
			return calendar.Page[calendar.EventV2]{}, err
		}
	}
	if request.CalendarID == "" {
		return s.searchEventsFanOut(ctx, request), nil
	}
	provider, rawCalendarID, err := s.registry.Resolve(request.CalendarID)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, invalidArgument(err.Error())
	}
	search, ok := provider.(calendar.SearchProviderV2)
	if !ok {
		return calendar.Page[calendar.EventV2]{}, unsupported(provider.Name(), request.CalendarID, "event search is not supported")
	}
	prefixedCalendarID := request.CalendarID
	request.CalendarID = rawCalendarID
	page, err := search.SearchEventsV2(ctx, request)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	normalizePage(&page, provider.Name(), prefixedCalendarID)
	return page, nil
}

func (s *Service) RespondToEvent(ctx context.Context, request calendar.RespondToEventRequestV2) (*calendar.OperationResult, error) {
	provider, rawRef, err := s.resolveEventRef(request.Ref)
	if err != nil {
		return nil, err
	}
	responder, ok := provider.(calendar.ResponseProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), request.Ref.CalendarID, "event responses are not supported")
	}
	prefixedCalendarID := request.Ref.CalendarID
	if err := s.validateNotifications(ctx, provider, rawRef.CalendarID, prefixedCalendarID, &request.Notifications); err != nil {
		return nil, err
	}
	request.Ref = rawRef
	result, err := responder.RespondToEventV2(ctx, request)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	normalizeOperation(result, provider.Name(), prefixedCalendarID)
	return result, nil
}

func (s *Service) MoveEvent(ctx context.Context, request calendar.MoveEventRequestV2) (*calendar.OperationResult, error) {
	provider, rawRef, err := s.resolveEventRef(request.Ref)
	if err != nil {
		return nil, err
	}
	destinationProvider, rawDestination, err := s.registry.Resolve(request.DestinationCalendarID)
	if err != nil {
		return nil, invalidArgument("invalid destination_calendar_id: " + err.Error())
	}
	if destinationProvider.Name() != provider.Name() {
		return nil, unsupported(provider.Name(), request.Ref.CalendarID, "cross-provider event moves are not supported")
	}
	mover, ok := provider.(calendar.MoveProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), request.Ref.CalendarID, "event moves are not supported")
	}
	prefixedDestination := request.DestinationCalendarID
	if err := s.validateNotifications(ctx, provider, rawRef.CalendarID, request.Ref.CalendarID, &request.Notifications); err != nil {
		return nil, err
	}
	request.Ref = rawRef
	request.DestinationCalendarID = rawDestination
	result, err := mover.MoveEventV2(ctx, request)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedDestination, err)
	}
	normalizeOperation(result, provider.Name(), prefixedDestination)
	return result, nil
}

func (s *Service) ImportEvent(ctx context.Context, request calendar.ImportEventRequestV2) (*calendar.EventV2, error) {
	if request.CalendarID == "" {
		return nil, invalidArgument("calendar_id is required")
	}
	if err := calendar.ValidateEventTimeRangeV2(request.Event.Start, request.Event.End); err != nil {
		return nil, invalidArgument(err.Error())
	}
	if err := calendar.ValidateRecurrence(request.Event.Recurrence); err != nil {
		return nil, &calendar.APIError{Code: calendar.ErrorInvalidRecurrence, Message: err.Error(), Cause: err}
	}
	provider, rawCalendarID, err := s.registry.Resolve(request.CalendarID)
	if err != nil {
		return nil, invalidArgument(err.Error())
	}
	importer, ok := provider.(calendar.ImportProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), request.CalendarID, "event import is not supported")
	}
	prefixedCalendarID := request.CalendarID
	request.CalendarID = rawCalendarID
	event, err := importer.ImportEventV2(ctx, request)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	normalizeEvent(event, provider.Name(), prefixedCalendarID)
	return event, nil
}

func (s *Service) searchEventsFanOut(ctx context.Context, request calendar.SearchEventsRequestV2) calendar.Page[calendar.EventV2] {
	results := make(chan fanOutResult)
	var wg sync.WaitGroup
	for _, provider := range s.registry.Providers() {
		provider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			search, ok := provider.(calendar.SearchProviderV2)
			if !ok {
				results <- failedSource(provider.Name(), "", fmt.Errorf("event search is not supported"))
				return
			}
			calendars, err := provider.ListCalendars(ctx)
			if err != nil {
				results <- failedSource(provider.Name(), "", err)
				return
			}
			for _, cal := range calendars {
				prefixedID := provider.Name() + ":" + cal.ID
				if s.registry.SkipInFanOut(prefixedID) {
					continue
				}
				providerRequest := request
				providerRequest.CalendarID = cal.ID
				page, err := search.SearchEventsV2(ctx, providerRequest)
				if err != nil {
					results <- failedSource(provider.Name(), prefixedID, err)
					continue
				}
				normalizePage(&page, provider.Name(), prefixedID)
				results <- fanOutResult{events: page.Items, status: calendar.SourceStatus{Provider: provider.Name(), CalendarID: prefixedID, Complete: page.Complete}}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	page := calendar.Page[calendar.EventV2]{Complete: true}
	for result := range results {
		page.Items = append(page.Items, result.events...)
		page.Sources = append(page.Sources, result.status)
		if !result.status.Complete {
			page.Complete = false
		}
	}
	return page
}
