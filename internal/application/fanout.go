package application

import (
	"context"
	"fmt"
	"sync"

	"calendar-mcp/internal/calendar"
)

type fanOutResult struct {
	events []calendar.EventV2
	status calendar.SourceStatus
}

func (s *Service) listEventsFanOut(ctx context.Context, request calendar.ListEventsRequestV2) calendar.Page[calendar.EventV2] {
	providers := s.registry.Providers()
	results := make(chan fanOutResult)
	var wg sync.WaitGroup
	for _, provider := range providers {
		provider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			calendars, err := provider.ListCalendars(ctx)
			if err != nil {
				results <- failedSource(provider.Name(), "", err)
				return
			}
			v2, ok := provider.(calendar.EventProviderV2)
			if !ok {
				results <- failedSource(provider.Name(), "", fmt.Errorf("V2 event reads are not supported"))
				return
			}
			for _, cal := range calendars {
				prefixedID := provider.Name() + ":" + cal.ID
				if s.registry.SkipInFanOut(prefixedID) {
					continue
				}
				providerRequest := request
				providerRequest.CalendarID = cal.ID
				page, err := v2.ListEventsV2(ctx, providerRequest)
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
	for item := range results {
		page.Items = append(page.Items, item.events...)
		page.Sources = append(page.Sources, item.status)
		if !item.status.Complete {
			page.Complete = false
		}
	}
	return page
}

func failedSource(provider, calendarID string, err error) fanOutResult {
	return fanOutResult{status: calendar.SourceStatus{
		Provider:   provider,
		CalendarID: calendarID,
		Complete:   false,
		Error:      &calendar.APIError{Code: calendar.ErrorProviderUnavailable, Message: err.Error(), Provider: provider, CalendarID: calendarID, Retryable: true, Cause: err},
	}}
}
