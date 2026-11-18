package apple

import (
	"context"
	"errors"

	"calendar-mcp/internal/calendar"
	"github.com/emersion/go-ical"
)

const appleGatewayWriteProperty = "X-CALENDAR-MCP-WRITE-DISPATCH"

func (p *Provider) FindGatewayWrite(ctx context.Context, calendarID, id string) (*calendar.EventV2, error) {
	if id == "" || calendarID == "" {
		return nil, errors.New("missing CalDAV recovery identity")
	}
	objects, err := p.getCalendarObjectsFallbackWithCompleteness(ctx, calendarID, true)
	if err != nil {
		return nil, err
	}
	var found *calendar.EventV2
	for _, object := range objects {
		if object.Data == nil {
			return nil, errors.New("missing CalDAV recovery representation")
		}
		for _, event := range object.Data.Events() {
			value, _ := event.Props.Text(appleGatewayWriteProperty)
			if value != id {
				continue
			}
			if found != nil {
				return nil, errors.New("ambiguous CalDAV write receipt")
			}
			if err := normalizeAppleFixedOffsetTimezone(&object); err != nil {
				return nil, err
			}
			result := appleEventV2(event, calendarID, object.Path, object.ETag)
			if prop := event.Props.Get(ical.PropRecurrenceID); prop != nil {
				original := appleEventTimeV2(prop)
				result.ID = appleInstanceID(object.Path, original)
				result.RecurringEventID = object.Path
				result.OriginalStart = &original
				result.InstanceKind = "exception"
			}
			if result.ID == "" || result.Status == "cancelled" {
				return nil, errors.New("invalid CalDAV write receipt")
			}
			found = &result
		}
	}
	return found, nil
}
