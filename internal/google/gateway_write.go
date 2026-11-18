package google

import (
	"context"
	"errors"

	"calendar-mcp/internal/calendar"
	gcal "google.golang.org/api/calendar/v3"
)

const googleGatewayWriteProperty = "calendarMcpWriteDispatch"

func setGatewayWriteMarker(event *gcal.Event, id string) {
	if id == "" {
		return
	}
	if event.ExtendedProperties == nil {
		event.ExtendedProperties = &gcal.EventExtendedProperties{}
	}
	if event.ExtendedProperties.Private == nil {
		event.ExtendedProperties.Private = map[string]string{}
	}
	event.ExtendedProperties.Private[googleGatewayWriteProperty] = id
}

func (p *Provider) FindGatewayWrite(ctx context.Context, calendarID, id string) (*calendar.EventV2, error) {
	if calendarID == "" || id == "" {
		return nil, errors.New("missing Google recovery identity")
	}
	var found *calendar.EventV2
	next := ""
	seen := map[string]bool{}
	for page := 0; page < 64; page++ {
		callCtx, observe, err := p.startRoleObservation(ctx, calendarID)
		if err != nil {
			return nil, err
		}
		call := p.svc.Events.List(calendarID).PrivateExtendedProperty(googleGatewayWriteProperty + "=" + id).ShowDeleted(false).SingleEvents(false).MaxResults(2)
		if next != "" {
			call = call.PageToken(next)
		}
		reply, err := call.Context(callCtx).Do()
		if err != nil {
			return nil, authorityError(callCtx, err)
		}
		if err := observeRole(callCtx, observe, reply.AccessRole); err != nil {
			return nil, err
		}
		if len(reply.Items) > 2 {
			return nil, errors.New("invalid Google recovery page")
		}
		for _, event := range reply.Items {
			if event == nil || event.Id == "" || event.ExtendedProperties == nil || event.ExtendedProperties.Private[googleGatewayWriteProperty] != id || event.Status == "cancelled" || found != nil {
				return nil, errors.New("ambiguous Google write receipt")
			}
			value := fromGoogleEventV2(event, calendarID, reply.TimeZone)
			found = &value
		}
		next = reply.NextPageToken
		if next == "" {
			return found, nil
		}
		if len(next) > 4096 || seen[next] {
			return nil, errors.New("invalid Google recovery continuation")
		}
		seen[next] = true
	}
	return nil, errors.New("Google recovery page limit exceeded")
}

func (p *Provider) FindGatewayPointWrite(ctx context.Context, ref calendar.EventRef, id string) (*calendar.EventV2, error) {
	callCtx, _, err := p.startRoleObservation(ctx, ref.CalendarID)
	if err != nil {
		return nil, err
	}
	event, err := p.svc.Events.Get(ref.CalendarID, ref.EventID).Context(callCtx).Do()
	if err != nil {
		return nil, authorityError(callCtx, err)
	}
	if event.Id != ref.EventID || event.Status == "cancelled" || event.ExtendedProperties == nil || event.ExtendedProperties.Private[googleGatewayWriteProperty] != id {
		return nil, nil
	}
	result := fromGoogleEventV2(event, ref.CalendarID, eventTimeZone(event.Start))
	return &result, nil
}
