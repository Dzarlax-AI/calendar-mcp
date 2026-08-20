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
