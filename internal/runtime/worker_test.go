package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/storage"
)

func TestWorkerRequiresStorage(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if err := Worker(context.Background()); !errors.Is(err, errWorkerNotConfigured) {
		t.Fatalf("Worker() error = %v, want %v", err, errWorkerNotConfigured)
	}
}

func TestMaintainJobLeaseRenewsUntilStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	renewed := make(chan time.Time, 1)
	go func() {
		done <- maintainJobLease(ctx, ticks, func(at time.Time) (bool, error) {
			renewed <- at
			return true, nil
		}, func() {})
	}()
	want := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	ticks <- want
	if got := <-renewed; !got.Equal(want) {
		t.Fatalf("renewed at %v, want %v", got, want)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMaintainJobLeaseCancelsWorkWhenOwnershipIsLost(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	workCtx, cancelWork := context.WithCancel(context.Background())
	err := maintainJobLease(context.Background(), ticks, func(time.Time) (bool, error) {
		return false, nil
	}, cancelWork)
	if !errors.Is(err, storage.ErrJobLeaseLost) {
		t.Fatalf("error = %v, want ErrJobLeaseLost", err)
	}
	if workCtx.Err() == nil {
		t.Fatal("work context was not cancelled")
	}
}

func TestMaintainJobLeaseSkipsRenewAfterShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	cancel()
	renewed := false

	err := maintainJobLease(ctx, ticks, func(time.Time) (bool, error) {
		renewed = true
		return true, nil
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if renewed {
		t.Fatal("lease renewed after shutdown")
	}
}

func TestMaintainJobLeaseIgnoresContextRenewalErrorDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	workCancelled := false

	err := maintainJobLease(ctx, ticks, func(time.Time) (bool, error) {
		cancel()
		return false, context.Canceled
	}, func() { workCancelled = true })
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if workCancelled {
		t.Fatal("work cancellation invoked for shutdown-related renewal error")
	}
}

func TestMaintainJobLeasePreservesUnrelatedRenewalErrorDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	want := errors.New("database unavailable")
	workCancelled := false

	err := maintainJobLease(ctx, ticks, func(time.Time) (bool, error) {
		cancel()
		return false, want
	}, func() { workCancelled = true })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if !workCancelled {
		t.Fatal("work cancellation not invoked for unrelated renewal error")
	}
}
