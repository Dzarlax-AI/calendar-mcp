package microsoft

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"calendar-mcp/internal/calendar"
)

const microsoftGatewayWriteProperty = "String {8ECCC264-6887-4EBE-992F-8888D0AB112E} Name CalendarMcpWriteDispatch"

func (p *Provider) FindGatewayWrite(ctx context.Context, calendarID, id string) (*calendar.EventV2, error) {
	if calendarID == "" || id == "" {
		return nil, errors.New("missing Graph recovery identity")
	}
	params := url.Values{"$filter": {fmt.Sprintf("singleValueExtendedProperties/Any(ep: ep/id eq '%s' and ep/value eq '%s')", microsoftGatewayWriteProperty, id)},
		"$expand": {fmt.Sprintf("singleValueExtendedProperties($filter=id eq '%s')", microsoftGatewayWriteProperty)}, "$top": {"2"}}
	next := p.baseURL + "/me/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
	seen := map[string]bool{}
	var found *calendar.EventV2
	for next != "" {
		if len(next) > 8192 || seen[next] || len(seen) >= 64 {
			return nil, errors.New("invalid Graph recovery continuation")
		}
		if err := p.validateGraphPageURL(next); err != nil {
			return nil, err
		}
		seen[next] = true
		var page struct {
			Value []graphEvent `json:"value"`
			Next  string       `json:"@odata.nextLink"`
		}
		if err := p.getURLWithHeaders(ctx, next, nil, &page); err != nil {
			return nil, err
		}
		if page.Value == nil || len(page.Value) > 2 {
			return nil, errors.New("invalid Graph recovery page")
		}
		for _, event := range page.Value {
			matches := 0
			for _, prop := range event.SingleValueExtendedProperties {
				if prop.ID == microsoftGatewayWriteProperty && prop.Value == id {
					matches++
				}
			}
			if matches != 1 || found != nil || event.ID == "" || event.IsCancelled {
				return nil, errors.New("ambiguous Graph write receipt")
			}
			value, err := event.toEventV2(calendarID)
			if err != nil {
				return nil, err
			}
			found = &value
		}
		next = page.Next
	}
	return found, nil
}

func (p *Provider) FindGatewayPointWrite(ctx context.Context, ref calendar.EventRef, id string) (*calendar.EventV2, error) {
	params := url.Values{"$expand": {fmt.Sprintf("singleValueExtendedProperties($filter=id eq '%s')", microsoftGatewayWriteProperty)}}
	path := "/me/calendars/" + url.PathEscape(ref.CalendarID) + "/events/" + url.PathEscape(ref.EventID)
	var event graphEvent
	if err := p.getWithParams(ctx, path, params, &event); err != nil {
		return nil, err
	}
	matches := 0
	for _, prop := range event.SingleValueExtendedProperties {
		if prop.ID == microsoftGatewayWriteProperty && prop.Value == id {
			matches++
		}
	}
	if event.ID != ref.EventID || matches != 1 || event.IsCancelled {
		return nil, nil
	}
	value, err := event.toEventV2(ref.CalendarID)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
