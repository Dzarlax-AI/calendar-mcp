package calendar

import (
	"context"
	"time"
)

// CalendarReadEvidence describes a completed provider-side calendar check.
// The provider must establish each stronger scope separately.
type CalendarReadEvidence struct {
	Calendar     Calendar
	Unknown      bool // Inventory entry exists, but its scoped read probe was inconclusive.
	Capabilities CalendarCapabilities
	GoogleRole   string
	ReadEvents   bool
	ReadPrivate  bool
}

type CalendarReadEvidenceProvider interface {
	CheckCalendarReadEvidence(context.Context) ([]CalendarReadEvidence, error)
}

func (c *CalendarReadEvidence) LimitOperations(write bool) {
	if !c.ReadEvents {
		c.Capabilities.Operations = OperationCapabilities{}
	} else {
		c.Capabilities.Operations.List, c.Capabilities.Operations.Get = true, true
		c.Capabilities.Operations.Search, c.Capabilities.Operations.Instances = true, true
	}
	if !write {
		ops := &c.Capabilities.Operations
		ops.Create, ops.Update, ops.Delete = false, false, false
		ops.Respond, ops.Move, ops.Import = false, false, false
	}
	c.Capabilities.ReadOnly = !write || !c.ReadEvents
}

type gatewayReadRestrictionKey struct{}

// WithGatewayReadRestriction connects a provider's affirmative restriction to
// the current background lease. Other provider callers have no callback.
func WithGatewayReadRestriction(ctx context.Context, callback func(context.Context, string, bool) error) context.Context {
	return context.WithValue(ctx, gatewayReadRestrictionKey{}, callback)
}

// RestrictGatewayCalendarRead applies a confirmed event denial or a confirmed
// downgrade from private access as soon as the provider observes it.
func RestrictGatewayCalendarRead(ctx context.Context, rawID string, eventsDenied bool) error {
	callback, _ := ctx.Value(gatewayReadRestrictionKey{}).(func(context.Context, string, bool) error)
	if callback == nil {
		return nil
	}
	return callback(ctx, rawID, eventsDenied)
}

// GatewayCalendarProbe limits one source's share of the account check.
// Leave time to classify the remaining sources even when one stalls.
func GatewayCalendarProbe(ctx context.Context, remaining int) (context.Context, context.CancelFunc) {
	budget := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/time.Duration(remaining+1))
	}
	return context.WithTimeout(ctx, budget)
}
