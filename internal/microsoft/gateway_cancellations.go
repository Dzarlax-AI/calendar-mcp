package microsoft

import (
	"context"
	"errors"
	"net/url"
	"time"

	"calendar-mcp/internal/calendar"
)

// calendarView only exposes surviving instances. The calendar's events
// collection lists masters independently of the occurrences in that window.
func (p *Provider) addGatewaySeriesInventory(ctx context.Context, calendarID string, masters map[string]struct{}) error {
	const maxPages, maxMasters = 128, 4096
	params := url.Values{"$select": {"id,type"}, "$top": {"250"}}
	next := p.baseURL + "/me/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
	visited := map[string]bool{}
	ids := map[string]bool{}
	for next != "" {
		if visited[next] || len(visited) >= maxPages || len(next) > 8192 {
			return errors.New("invalid Graph series inventory continuation")
		}
		if err := p.validateGraphPageURL(next); err != nil {
			return err
		}
		visited[next] = true
		var page struct {
			Value []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"value"`
			Next string `json:"@odata.nextLink"`
		}
		if err := p.getURLWithHeaders(ctx, next, nil, &page); err != nil {
			return err
		}
		if page.Value == nil {
			return errors.New("incomplete Graph series inventory")
		}
		for _, item := range page.Value {
			if item.ID == "" || ids[item.ID] {
				return errors.New("ambiguous Graph series inventory")
			}
			ids[item.ID] = true
			switch item.Type {
			case "seriesMaster":
				masters[item.ID] = struct{}{}
			case "singleInstance":
			default:
				return errors.New("unknown Graph event type in series inventory")
			}
			if len(masters) > maxMasters {
				return errors.New("Graph series inventory exceeds its limit")
			}
		}
		next = page.Next
	}
	return nil
}

func gatewayCancelledDateOverlaps(value *calendar.EventTime, zone string, start, end time.Time) bool {
	if value == nil || !value.IsAllDay() {
		return false
	}
	if location := calendar.CollectionLocation("microsoft", zone); location != nil {
		day, err := time.ParseInLocation(calendar.DateLayout, value.Date, location)
		return err == nil && day.Before(end) && day.AddDate(0, 0, 1).After(start)
	}
	// Keep boundary candidates when Graph omitted the recurrence timezone.
	// Without a timezone the reader cannot attest this civil range as covered.
	return value.Date >= start.UTC().Add(-24*time.Hour).Format(calendar.DateLayout) &&
		value.Date <= end.UTC().Add(24*time.Hour).Format(calendar.DateLayout)
}
