package microsoft

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"calendar-mcp/internal/calendar"
)

type graphReadEvidenceCalendar struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Color               string `json:"color"`
	CanEdit             bool   `json:"canEdit"`
	CanViewPrivateItems bool   `json:"canViewPrivateItems"`
	IsDefaultCalendar   bool   `json:"isDefaultCalendar"`
}

func (p *Provider) CheckCalendarReadEvidence(ctx context.Context) ([]calendar.CalendarReadEvidence, error) {
	params := url.Values{"$select": {"id,name,color,canEdit,canViewPrivateItems,isDefaultCalendar"}}
	next := p.baseURL + "/me/calendars?" + params.Encode()
	visited, ids := map[string]bool{}, map[string]bool{}
	items := []graphReadEvidenceCalendar{}
	for next != "" {
		if visited[next] || len(visited) >= 64 {
			return nil, fmt.Errorf("incomplete Graph calendar inventory")
		}
		visited[next] = true
		var page struct {
			Value *[]graphReadEvidenceCalendar `json:"value"`
			Next  string                       `json:"@odata.nextLink"`
		}
		if err := p.getURLWithHeaders(ctx, next, nil, &page); err != nil {
			return nil, err
		}
		if page.Value == nil || len(items)+len(*page.Value) > 256 {
			return nil, fmt.Errorf("invalid Graph calendar inventory")
		}
		for _, item := range *page.Value {
			if item.ID == "" || ids[item.ID] {
				return nil, fmt.Errorf("invalid Graph calendar identity")
			}
			ids[item.ID] = true
			// A restriction observed on this page must survive a later
			// inventory or event-probe failure.
			if !item.CanViewPrivateItems {
				if err := calendar.RestrictGatewayCalendarRead(ctx, item.ID, false); err != nil {
					return nil, err
				}
			}
			items = append(items, item)
		}
		next = page.Next
	}
	result := make([]calendar.CalendarReadEvidence, 0, len(items))
	start := time.Now().UTC().Truncate(time.Minute)
	probe := url.Values{
		"startDateTime": {start.Format(time.RFC3339)}, "endDateTime": {start.Add(time.Minute).Format(time.RFC3339)},
		"$select": {"id"}, "$top": {"1"},
	}
	for i, item := range items {
		path := "/me/calendars/" + url.PathEscape(item.ID) + "/calendarView"
		var page struct {
			Value *[]struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		probeCtx, cancel := calendar.GatewayCalendarProbe(ctx, len(items)-i)
		err := p.getWithParams(probeCtx, path, probe, &page)
		cancel()
		if err != nil && !graphCalendarReadDenied(err) {
			result = append(result, calendar.CalendarReadEvidence{Calendar: calendar.Calendar{ID: item.ID}, Unknown: true})
			continue
		}
		if err == nil && (page.Value == nil || len(*page.Value) > 1 || len(*page.Value) == 1 && (*page.Value)[0].ID == "") {
			result = append(result, calendar.CalendarReadEvidence{Calendar: calendar.Calendar{ID: item.ID}, Unknown: true})
			continue
		}
		read := err == nil
		if !read {
			if err := calendar.RestrictGatewayCalendarRead(ctx, item.ID, true); err != nil {
				return nil, err
			}
		}
		proof := calendar.CalendarReadEvidence{
			Calendar:     calendar.Calendar{ID: item.ID, Name: item.Name, Color: item.Color, Primary: item.IsDefaultCalendar, ReadOnly: !item.CanEdit || !read},
			Capabilities: graphCapabilities(item.CanEdit), ReadEvents: read, ReadPrivate: read && item.CanViewPrivateItems,
		}
		proof.LimitOperations(item.CanEdit && read)
		result = append(result, proof)
	}
	return result, nil
}

func graphCalendarReadDenied(err error) bool {
	var api *calendar.APIError
	if !errors.As(err, &api) {
		return false
	}
	// The existing Graph adapter groups 401 and 403. A source-level access
	// denial applies only to this calendar, including an object-level 404.
	return api.Code == calendar.ErrorPermissionDenied || api.Code == calendar.ErrorNotFound
}
