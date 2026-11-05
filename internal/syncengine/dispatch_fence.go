package syncengine

import (
	"context"
	"errors"
)

// DispatchFence checks whether the current job attempt may start a provider
// mutation. It is a point-in-time check; it cannot revoke an HTTP request
// already sent or guarantee that the lease remains valid after returning.
type DispatchFence interface {
	CheckProviderDispatch(context.Context) error
}

var ErrDispatchFenceUnavailable = errors.New("provider dispatch fence unavailable")

// CheckDispatchFence fails closed when no job fence is installed. A successful
// check does not replace durable operation intent or outcome reconciliation.
func CheckDispatchFence(ctx context.Context, fence DispatchFence) error {
	if fence == nil {
		return ErrDispatchFenceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fence.CheckProviderDispatch(ctx)
}
