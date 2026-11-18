package calendar

import "context"

const GatewayWritePendingWarning = "Calendar changes are awaiting local publication."

// GatewayWriteScope contains identity only. The original mutation or
// notification request is never retained for replay.
type GatewayWriteScope struct {
	Kind, CalendarID, DestinationCalendarID, EventID, AuthorizationRevision string
	Scope                                                                   MutationScope
}

type gatewayWriteContextKey struct{}

type GatewayWriteTracking struct {
	scope      GatewayWriteScope
	dispatchID string
	pending    bool
}

func EnableGatewayWriteTracking(ctx context.Context) (context.Context, *GatewayWriteTracking) {
	tracking := &GatewayWriteTracking{}
	return context.WithValue(ctx, gatewayWriteContextKey{}, tracking), tracking
}

func DescribeGatewayWrite(ctx context.Context, provider Provider, scope GatewayWriteScope) {
	t, _ := ctx.Value(gatewayWriteContextKey{}).(*GatewayWriteTracking)
	if t == nil {
		return
	}
	if bound, ok := provider.(interface{ CredentialBinding() (string, [32]byte) }); ok {
		scope.AuthorizationRevision, _ = bound.CredentialBinding()
	}
	t.scope = scope
}

func GatewayWriteDispatchID(ctx context.Context) string {
	t, _ := ctx.Value(gatewayWriteContextKey{}).(*GatewayWriteTracking)
	if t == nil {
		return ""
	}
	return t.dispatchID
}

func (t *GatewayWriteTracking) PendingPublication() bool { return t.pending }

type GatewayWriteReservation interface {
	ID() string
	Authorize(context.Context) error
	Complete(context.Context, any, bool) error
}

type GatewayWriteDispatchGuard interface {
	BeginGatewayWrite(context.Context, []string, GatewayWriteScope) (GatewayWriteReservation, error)
}

type GatewayWriteLookupProvider interface {
	FindGatewayWrite(context.Context, string, string) (*EventV2, error)
}

func GatewayUpdateDispatchID(ctx context.Context) string {
	t, _ := ctx.Value(gatewayWriteContextKey{}).(*GatewayWriteTracking)
	if t == nil || t.scope.Kind != "update" || t.scope.Scope == ScopeFollowing {
		return ""
	}
	return t.dispatchID
}

type GatewayPointWriteLookupProvider interface {
	FindGatewayPointWrite(context.Context, EventRef, string) (*EventV2, error)
}
