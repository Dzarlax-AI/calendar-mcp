package application

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"calendar-mcp/internal/calendar"
)

type fanOutResult struct {
	events []calendar.EventV2
	status calendar.SourceStatus
}

const maxFanOutPages = 1000

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
				providerRequest.PageToken = ""
				page, err := drainEventPages(ctx, providerRequest, v2.ListEventsV2)
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
	sortFanOutPage(&page)
	return page
}

func drainEventPages(ctx context.Context, request calendar.ListEventsRequestV2, fetch func(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error)) (calendar.Page[calendar.EventV2], error) {
	result := calendar.Page[calendar.EventV2]{Complete: true}
	seen := make(map[string]struct{})
	for pageNumber := 0; pageNumber < maxFanOutPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		page, err := fetch(ctx, request)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, page.Items...)
		result.Complete = result.Complete && page.Complete
		if page.NextPageToken == "" {
			return result, nil
		}
		if _, duplicate := seen[page.NextPageToken]; duplicate {
			return result, fmt.Errorf("provider pagination repeated page token")
		}
		seen[page.NextPageToken] = struct{}{}
		request.PageToken = page.NextPageToken
	}
	return result, fmt.Errorf("provider pagination exceeded %d pages", maxFanOutPages)
}

func sortFanOutPage(page *calendar.Page[calendar.EventV2]) {
	sort.SliceStable(page.Items, func(i, j int) bool {
		left, right := page.Items[i], page.Items[j]
		leftStart, rightStart := left.Start.Date+left.Start.DateTime, right.Start.Date+right.Start.DateTime
		if leftStart != rightStart {
			return leftStart < rightStart
		}
		if left.CalendarID != right.CalendarID {
			return left.CalendarID < right.CalendarID
		}
		return left.ID < right.ID
	})
	sort.SliceStable(page.Sources, func(i, j int) bool {
		if page.Sources[i].Provider != page.Sources[j].Provider {
			return page.Sources[i].Provider < page.Sources[j].Provider
		}
		return page.Sources[i].CalendarID < page.Sources[j].CalendarID
	})
}

func failedSource(provider, calendarID string, err error) fanOutResult {
	return fanOutResult{status: calendar.SourceStatus{
		Provider:   provider,
		CalendarID: calendarID,
		Complete:   false,
		Error:      &calendar.APIError{Code: calendar.ErrorProviderUnavailable, Message: err.Error(), Provider: provider, CalendarID: calendarID, Retryable: true, Cause: err},
	}}
}
