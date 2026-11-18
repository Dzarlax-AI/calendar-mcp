package cachedpages

import (
	"context"
	"slices"

	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func (s *Service) GatewayListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	if !s.gatewayCaller(ctx) {
		return nil, ErrUnavailable
	}
	proofs, err := s.store.GatewayCalendarInventory(ctx, s.now())
	if err != nil {
		return nil, ErrUnavailable
	}
	result := make([]calendar.Calendar, 0, len(proofs))
	for _, proof := range proofs {
		result = append(result, proof.Metadata)
	}
	current, err := s.store.GatewayCalendarInventory(ctx, s.now())
	if err != nil || !slices.EqualFunc(proofs, current, sameGatewayCalendarGrant) {
		return nil, ErrUnavailable
	}
	return result, nil
}

func (s *Service) GatewayCapabilities(ctx context.Context, reference string) (calendar.CalendarCapabilities, error) {
	if !s.gatewayCaller(ctx) {
		return calendar.CalendarCapabilities{}, ErrUnavailable
	}
	if reference == "" || len(reference) > maxCursorIDBytes {
		return calendar.CalendarCapabilities{}, ErrInvalidArgument
	}
	first, err := s.store.GatewayCalendarGrantByReference(ctx, reference, s.now())
	if err != nil {
		return calendar.CalendarCapabilities{}, ErrUnavailable
	}
	result := first.Capabilities
	result.CalendarID = reference
	second, err := s.store.GatewayCalendarGrantByReference(ctx, reference, s.now())
	if err != nil || !sameGatewayCalendarGrant(first, second) {
		return calendar.CalendarCapabilities{}, ErrUnavailable
	}
	return result, nil
}

func (s *Service) gatewayCaller(ctx context.Context) bool {
	_, verified := auth.VerifiedCallerFromContext(ctx)
	return verified && s.Ready()
}

func sameGatewayCalendarGrant(a, b storage.GatewayCalendarGrant) bool {
	return a.Metadata.ID == b.Metadata.ID && a.Revision == b.Revision &&
		a.ConnectionRevision == b.ConnectionRevision && a.DetailReceipt == b.DetailReceipt
}
