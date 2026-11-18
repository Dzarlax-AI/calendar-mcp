package microsoft

import (
	"context"
	"errors"
	"net/url"
	"sort"
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

// gatewayCancellationEvidence enriches an already collected calendarView.
// Master reads are optional: reserve time to publish the ordinary snapshot
// even if the wider inventory or a master stalls. Positive access denial
// still invalidates the attempt.
func (p *Provider) gatewayCancellationEvidence(ctx context.Context, request calendar.ListEventsRequestV2,
	active map[string]struct{}, present, cancelled map[string]bool) ([]calendar.EventV2, map[string]string, error) {
	budget := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	probe, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var items []calendar.EventV2
	verified := map[string]string{}
	fetch := func(masters map[string]struct{}) error {
		ids := make([]string, 0, len(masters))
		for id := range masters {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if probe.Err() != nil {
				break
			}
			master, err := p.getSeriesMaster(probe, request.CalendarID, id)
			if err != nil {
				var api *calendar.APIError
				if errors.As(err, &api) && api.Code == calendar.ErrorPermissionDenied {
					return err
				}
				continue
			}
			if master.ID != id || master.Type != "seriesMaster" {
				continue
			}
			masterEvent, err := master.toEventV2(request.CalendarID)
			if err != nil {
				continue
			}
			items = append(items, masterEvent)
			zone := ""
			if master.Recurrence != nil && master.Recurrence.Range.RecurrenceTimeZone != "" {
				zone, err = graphIANAZone(master.Recurrence.Range.RecurrenceTimeZone)
				if err != nil {
					continue
				}
			}
			complete := master.CancelledOccurrences != nil
			seen := map[string]bool{}
			for _, occurrenceID := range master.CancelledOccurrences {
				if seen[occurrenceID] {
					continue
				}
				seen[occurrenceID] = true
				if present[occurrenceID] {
					complete = complete && cancelled[occurrenceID]
					continue
				}
				if !master.IsAllDay && zone == "" {
					complete = false
					continue
				}
				item, err := cancelledOccurrence(*master, request.CalendarID, occurrenceID)
				if err != nil {
					complete = false
					continue
				}
				if eventTimeInWindow(item.OriginalStart, request.Start, request.End) ||
					gatewayCancelledDateOverlaps(item.OriginalStart, zone, request.Start, request.End) {
					item.Visibility = master.Sensitivity
					items = append(items, item)
				}
			}
			if complete {
				verified[id] = zone
			}
		}
		return nil
	}
	if err := fetch(active); err != nil {
		return nil, nil, err
	}
	if probe.Err() == nil {
		additional := map[string]struct{}{}
		if err := p.addGatewaySeriesInventory(probe, request.CalendarID, additional); err == nil {
			for id := range active {
				delete(additional, id)
			}
			if err := fetch(additional); err != nil {
				return nil, nil, err
			}
		} else if graphCalendarReadDenied(err) {
			return nil, nil, err
		}
	}
	return items, verified, ctx.Err()
}
