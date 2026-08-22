package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/storage"
)

type workerPolicyProvider struct {
	calendar.Provider
	name   string
	policy calendar.EventSyncPolicy
}

func (p workerPolicyProvider) Name() string { return p.name }

func (p workerPolicyProvider) EventSyncPolicy() calendar.EventSyncPolicy { return p.policy }

type emptyProviderBuilder struct{}

func (emptyProviderBuilder) Build(context.Context) ([]calendar.Provider, error) { return nil, nil }

func TestWorkerRequiresStorage(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if err := Worker(context.Background()); !errors.Is(err, errWorkerNotConfigured) {
		t.Fatalf("Worker() error = %v, want %v", err, errWorkerNotConfigured)
	}
}

func TestRunOneEventSyncDoesNothingWhenFeatureIsDisabled(t *testing.T) {
	// A nil store/factory would panic if the read model attempted any storage
	// or provider work; disabled mode must remain inert for existing workers.
	err := runOneEventSync(context.Background(), nil, nil, &config.Config{EventReadModelEnabled: false}, time.Now().UTC())
	if err != nil {
		t.Fatalf("runOneEventSync() error = %v", err)
	}
}

func TestEventReadModelWindowUsesUTCDayAndRebasesStatesDaily(t *testing.T) {
	cfg := &config.Config{EventReadModelEnabled: true, EventCacheLookbackDays: 365, EventCacheLookaheadDays: 730}
	firstTick := time.Date(2026, 8, 22, 2, 1, 0, 0, time.FixedZone("CEST", 2*60*60))
	secondTick := time.Date(2026, 8, 22, 23, 59, 0, 0, time.FixedZone("CEST", 2*60*60))
	firstWindow := eventReadModelWindow(cfg, firstTick)
	secondWindow := eventReadModelWindow(cfg, secondTick)
	if firstWindow != secondWindow {
		t.Fatalf("same UTC day windows differ: %#v != %#v", firstWindow, secondWindow)
	}
	if want := time.Date(2025, 8, 22, 0, 0, 0, 0, time.UTC); !firstWindow.Start.Equal(want) {
		t.Fatalf("window start = %s, want %s", firstWindow.Start, want)
	}

	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	anchor := firstTick.UTC()
	if err := store.CreateConnection(ctx, storage.Connection{ID: "connection", Provider: "google", DisplayName: "Google", Status: "connected", EncryptedCredentials: []byte("cipher"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: anchor, UpdatedAt: anchor}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(ctx, storage.Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary", CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: anchor}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, anchor, firstWindow); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueCalendarSync(ctx, "one", anchor, anchor.Add(time.Hour))
	if err != nil || claimed == nil {
		t.Fatalf("first claim = %#v, err=%v", claimed, err)
	}
	next := anchor.Add(2 * time.Hour)
	if err := store.ApplyEventSyncPage(ctx, *claimed, storage.EventSyncBatch{NextCursor: "saved", NextSyncAt: &next}, true, anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureCalendarSyncStates(ctx, secondTick.UTC(), secondWindow); err != nil {
		t.Fatal(err)
	}
	sameDay, err := store.ClaimDueCalendarSync(ctx, "two", next, next.Add(time.Hour))
	if err != nil || sameDay == nil {
		t.Fatalf("same-day claim = %#v, err=%v", sameDay, err)
	}
	if sameDay.Cursor != "saved" || sameDay.Generation != claimed.Generation+1 {
		t.Fatalf("same-day state = %#v; cursor/generation were unexpectedly reset", sameDay)
	}
	nextDay := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if err := store.FailCalendarSync(ctx, *sameDay, "transient", next, nextDay); err != nil {
		t.Fatal(err)
	}
	nextWindow := eventReadModelWindow(cfg, nextDay)
	if !nextWindow.Start.Equal(firstWindow.Start.AddDate(0, 0, 1)) || !nextWindow.End.Equal(firstWindow.End.AddDate(0, 0, 1)) {
		t.Fatalf("next-day window = %#v, want one-day shift from %#v", nextWindow, firstWindow)
	}
	if err := store.EnsureCalendarSyncStates(ctx, nextDay, nextWindow); err != nil {
		t.Fatal(err)
	}
	nextDayState, err := store.ClaimDueCalendarSync(ctx, "three", nextDay, nextDay.Add(time.Hour))
	if err != nil || nextDayState == nil {
		t.Fatalf("next-day claim = %#v, err=%v", nextDayState, err)
	}
	if nextDayState.Cursor != "" || nextDayState.Generation != sameDay.Generation+2 {
		t.Fatalf("next-day state = %#v; want reset cursor and generation %d", nextDayState, sameDay.Generation+2)
	}
}

func TestDisabledReadModelIgnoresInvalidUnusedSettingsAndRunsRuleCycle(t *testing.T) {
	t.Setenv("EVENT_READ_MODEL_ENABLED", "false")
	t.Setenv("EVENT_SYNC_GOOGLE_INTERVAL", "not-a-duration")
	cfg := config.Load()
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// This reaches the existing stale-recovery and due-rule scheduling path;
	// disabled read-model parsing must not prevent legacy worker operation.
	if err := runWorkerCycleWithConfig(ctx, store, emptyProviderBuilder{}, cfg, time.Now().UTC()); err != nil {
		t.Fatalf("disabled worker cycle error = %v", err)
	}
}

func TestEnabledReadModelRejectsInvalidSettings(t *testing.T) {
	t.Setenv("EVENT_READ_MODEL_ENABLED", "true")
	t.Setenv("EVENT_SYNC_GOOGLE_INTERVAL", "not-a-duration")
	cfg := config.Load()
	if err := runWorkerCycleWithConfig(context.Background(), nil, nil, cfg, time.Now().UTC()); err == nil {
		t.Fatal("enabled worker cycle accepted invalid read-model settings")
	}
}

func TestEventSyncPolicyForKeepsAdapterBoundsAndOverridesCadence(t *testing.T) {
	cfg := &config.Config{
		EventSyncGoogleInterval:    73 * time.Second,
		EventSyncMicrosoftInterval: 74 * time.Second,
		EventSyncAppleInterval:     75 * time.Second,
	}
	adapter := workerPolicyProvider{name: "google", policy: calendar.EventSyncPolicy{
		PollInterval: 5 * time.Minute, RetryBase: 3 * time.Second, RetryMax: 9 * time.Minute, MaxPages: 33, MaxResets: 4,
	}}
	got := eventSyncPolicyFor(cfg)(adapter)
	if got.PollInterval != 73*time.Second || got.RetryBase != adapter.policy.RetryBase || got.RetryMax != adapter.policy.RetryMax || got.MaxPages != adapter.policy.MaxPages || got.MaxResets != adapter.policy.MaxResets {
		t.Fatalf("policy = %#v, want adapter policy with Google cadence override", got)
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

func TestMaintainCalendarSyncLeaseCancelsWorkWhenLeaseIsLost(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	workCtx, cancelWork := context.WithCancel(context.Background())
	err := maintainCalendarSyncLease(context.Background(), ticks, func(time.Time) error {
		return storage.ErrCalendarSyncLeaseLost
	}, cancelWork)
	if !errors.Is(err, storage.ErrCalendarSyncLeaseLost) {
		t.Fatalf("error = %v, want ErrCalendarSyncLeaseLost", err)
	}
	if workCtx.Err() == nil {
		t.Fatal("work context was not cancelled")
	}
}

func TestMaintainCalendarSyncLeaseRenewsUntilStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	renewed := make(chan time.Time, 1)
	go func() {
		done <- maintainCalendarSyncLease(ctx, ticks, func(at time.Time) error {
			renewed <- at
			return nil
		}, func() {})
	}()
	want := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ticks <- want
	if got := <-renewed; !got.Equal(want) {
		t.Fatalf("renewed at %v, want %v", got, want)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMaintainCalendarSyncLeaseIgnoresRenewalRaceAfterStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	workCancelled := false
	err := maintainCalendarSyncLease(ctx, ticks, func(time.Time) error {
		cancel()
		return storage.ErrCalendarSyncLeaseLost
	}, func() { workCancelled = true })
	if err != nil || workCancelled {
		t.Fatalf("error=%v workCancelled=%v", err, workCancelled)
	}
}
