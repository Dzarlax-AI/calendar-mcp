package runtime

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/syncengine"
)

type fakeJobDispatchReader struct {
	got storage.Job
	err error
}

func (f *fakeJobDispatchReader) CheckJobDispatchLease(_ context.Context, job storage.Job) error {
	f.got = job
	return f.err
}
func (f *fakeJobDispatchReader) CheckSyncTargetEligibility(context.Context, string) error { return nil }

func TestJobDispatchFenceUsesExactAttempt(t *testing.T) {
	job := storage.Job{ID: "synthetic", RuleID: "rule", ClaimedBy: "worker", Attempt: 3}
	reader := &fakeJobDispatchReader{}
	fence := newJobDispatchFence(reader, job)
	if err := syncengine.CheckDispatchFence(t.Context(), fence); err != nil || reader.got != job {
		t.Fatalf("check: %v, %+v", err, reader.got)
	}
	reader.err = storage.ErrJobLeaseLost
	if err := syncengine.CheckDispatchFence(t.Context(), fence); !errors.Is(err, storage.ErrJobLeaseLost) {
		t.Fatalf("lost lease: %v", err)
	}
	if err := syncengine.CheckDispatchFence(t.Context(), nil); !errors.Is(err, syncengine.ErrDispatchFenceUnavailable) {
		t.Fatalf("nil fence: %v", err)
	}
	if err := syncengine.CheckDispatchFence(t.Context(), newJobDispatchFence(nil, job)); !errors.Is(err, syncengine.ErrDispatchFenceUnavailable) {
		t.Fatalf("nil store: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := syncengine.CheckDispatchFence(ctx, fence); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
