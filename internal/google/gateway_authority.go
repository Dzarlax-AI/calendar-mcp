package google

import (
	"context"
	"fmt"

	"calendar-mcp/internal/calendar"
)

func (p *Provider) CheckCalendarReadEvidence(ctx context.Context) ([]calendar.CalendarReadEvidence, error) {
	items, err := p.discoverDetailCalendars(ctx, func(callCtx context.Context, id, role string) error {
		switch role {
		case "freeBusyReader":
			return calendar.RestrictGatewayCalendarRead(callCtx, id, true)
		case "reader", "writerWithoutPrivateAccess":
			return calendar.RestrictGatewayCalendarRead(callCtx, id, false)
		default:
			return nil
		}
	})
	if err != nil {
		return nil, classifyGoogleSyncError(err)
	}
	result := make([]calendar.CalendarReadEvidence, 0, len(items))
	for _, item := range items {
		proof := calendar.CalendarReadEvidence{
			Calendar: item.Calendar, GoogleRole: item.AccessRole,
			Capabilities: calendarCapabilities(item.AccessRole, item.Calendar.Primary || item.Calendar.ID == "primary"),
		}
		switch item.AccessRole {
		case "owner", "writer":
			proof.ReadEvents, proof.ReadPrivate = true, true
		case "reader", "writerWithoutPrivateAccess":
			proof.ReadEvents = true
		case "freeBusyReader":
		default:
			return nil, fmt.Errorf("unrecognized Google calendar access role")
		}
		proof.LimitOperations(proof.Capabilities.Operations.Create)
		result = append(result, proof)
	}
	return result, nil
}
