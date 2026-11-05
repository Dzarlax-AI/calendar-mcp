package runtime

import (
	"context"

	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/syncengine"
)

type jobDispatchLeaseReader interface {
	CheckJobDispatchLease(context.Context, storage.Job) error
	CheckSyncTargetEligibility(context.Context, string) error
}

// jobDispatchFence adapts the exact claimed job attempt to the engine. It
// is deliberately not installed by itself; worker wiring is separate.
type jobDispatchFence struct {
	store jobDispatchLeaseReader
	job   storage.Job
}

var _ syncengine.DispatchFence = (*jobDispatchFence)(nil)

func newJobDispatchFence(store jobDispatchLeaseReader, job storage.Job) *jobDispatchFence {
	return &jobDispatchFence{store: store, job: job}
}

func (f *jobDispatchFence) CheckProviderDispatch(ctx context.Context) error {
	if f == nil || f.store == nil {
		return syncengine.ErrDispatchFenceUnavailable
	}
	if err := f.store.CheckJobDispatchLease(ctx, f.job); err != nil {
		return err
	}
	return f.store.CheckSyncTargetEligibility(ctx, f.job.RuleID)
}
