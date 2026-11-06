package syncengine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type reviewDispatchFence struct {
	calls int
	err   error
}

func (f *reviewDispatchFence) CheckProviderDispatch(context.Context) error { f.calls++; return f.err }

func TestDispatchFenceProtectsEachProviderMutation(t *testing.T) {
	source := &fakeV2Provider{name: "google"}
	target := &fakeV2Provider{name: "apple"}
	guard := selectionGuardedTarget{EventProviderV2: target, registry: calendar.NewRegistry([]calendar.Provider{source, target}), sourceID: "google:source", targetID: "apple:target"}
	calls := []func() error{
		func() error { _, err := guard.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{}); return err },
		func() error { _, err := guard.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{}); return err },
		func() error { _, err := guard.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{}); return err },
	}
	for _, call := range calls {
		if err := call(); !errors.Is(err, ErrDispatchFenceUnavailable) {
			t.Fatalf("nil fence allowed mutation: %v", err)
		}
	}
	denied := &reviewDispatchFence{err: storage.ErrJobLeaseLost}
	guard.dispatchFence = denied
	for _, call := range calls {
		if err := call(); !errors.Is(err, storage.ErrJobLeaseLost) {
			t.Fatalf("lost lease allowed mutation: %v", err)
		}
	}
	if denied.calls != 3 || target.created != 0 || target.updated != 0 || target.deleted != 0 {
		t.Fatalf("denied calls=%d creates=%d updates=%d deletes=%d", denied.calls, target.created, target.updated, target.deleted)
	}
	allowed := &reviewDispatchFence{}
	guard.dispatchFence = allowed
	for _, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("valid fence rejected: %v", err)
		}
	}
	if allowed.calls != 3 || target.created != 1 || target.updated != 1 || target.deleted != 1 {
		t.Fatalf("allowed calls=%d creates=%d updates=%d deletes=%d", allowed.calls, target.created, target.updated, target.deleted)
	}
}

func TestDryRunDoesNotRequireDispatchFence(t *testing.T) {
	start := calendar.EventTime{Date: "2026-10-01"}
	event := calendar.EventV2{ID: "event", Title: "Meeting", Start: start, End: calendar.EventTime{Date: "2026-10-02"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
	target := &fakeV2Provider{name: "apple"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, true)
	if err != nil || result.Created != 1 {
		t.Fatalf("dry run: %+v %v", result, err)
	}
	if len(mappings.values) != 0 || target.created != 0 || target.updated != 0 || target.deleted != 0 {
		t.Fatal("dry run changed target or mapping")
	}
}

func TestExpiredFencePreservesMappedEvents(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		event := calendar.EventV2{ID: "event", Title: "changed", Start: calendar.EventTime{Date: "2026-10-01"}, End: calendar.EventTime{Date: "2026-10-02"}}
		if cancelled {
			event.Status = "cancelled"
		}
		source := &fakeV2Provider{name: "google", events: []calendar.EventV2{event}}
		target := &fakeV2Provider{name: "apple"}
		id := newMappingID("rule", event.ID, "")
		original := storage.Mapping{ID: id, RuleID: "rule", SourceEventID: event.ID, ObjectKind: "event", TargetEventID: "existing", ContentHash: "old", ReconciliationState: "current", Version: 1, Incarnation: "synthetic-incarnation"}
		mappings := &fakeMappings{values: map[string]storage.Mapping{id: original}}
		engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings).WithDispatchFence(&reviewDispatchFence{err: storage.ErrJobLeaseLost})
		rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
		if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, storage.ErrJobLeaseLost) {
			t.Fatalf("cancelled=%v: %v", cancelled, err)
		}
		if got := mappings.values[id]; got != original {
			t.Fatalf("cancelled=%v: mapping changed: %+v", cancelled, got)
		}
		if target.updated != 0 || target.deleted != 0 {
			t.Fatal("expired attempt called provider")
		}
	}
}

// A successful point-in-time check cannot establish that a later pause has
// prevented a provider call: the worker can be delayed between the check and
// dispatch. A stop protocol must account for that outstanding attempt.
func TestDispatchFenceDoesNotRevokeAnAlreadyCheckedCall(t *testing.T) {
	checked := make(chan struct{})
	proceed := make(chan struct{})
	target := &fakeV2Provider{name: "apple"}
	guard := selectionGuardedTarget{
		EventProviderV2: target,
		registry:        calendar.NewRegistry([]calendar.Provider{&fakeV2Provider{name: "google"}, target}),
		sourceID:        "google:source", targetID: "apple:target",
		dispatchFence: &reviewDispatchFence{},
		onDispatch:    func() { close(checked); <-proceed },
	}
	result := make(chan error, 1)
	go func() {
		_, err := guard.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{})
		result <- err
	}()
	<-checked
	// A pause committed here cannot undo the already successful check.
	close(proceed)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if target.created != 1 {
		t.Fatalf("expected the already checked fake call to proceed, got %d", target.created)
	}
}

type realJobFence struct {
	store *storage.Store
	job   storage.Job
}

func (f realJobFence) CheckProviderDispatch(ctx context.Context) error {
	return f.store.CheckJobDispatchLease(ctx, f.job)
}

func TestPauseWaitsForCallDelayedAfterFence(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "pause-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateConnection(ctx, storage.Connection{ID: "account", Provider: "google", AccountFingerprint: "account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err = store.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.CreateRule(ctx, storage.Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = store.EnqueueJob(ctx, storage.Job{ID: "job", RuleID: "rule", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimJob(ctx, "worker", time.Now().UTC())
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err = store.StartRun(ctx, storage.Run{ID: "run", JobID: job.ID, RuleID: job.RuleID, Trigger: "manual", Outcome: "running", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	target := &fakeV2Provider{name: "apple"}
	checked := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan error, 1)
	guard := selectionGuardedTarget{EventProviderV2: target, registry: calendar.NewRegistry([]calendar.Provider{&fakeV2Provider{name: "google"}, target}), sourceID: "google:source", targetID: "apple:target", dispatchFence: realJobFence{store, *job}, onDispatch: func() { close(checked); <-resume }}
	go func() { _, e := guard.CreateEventV2(ctx, calendar.CreateEventRequestV2{}); done <- e }()
	<-checked
	if err = store.RequestRulePause(ctx, "rule", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if status, e := store.RuleControlStatus(ctx, "rule"); e != nil || status != "pausing" {
		t.Fatalf("status while call may still start: %s %v", status, e)
	}
	if err = store.ArchiveRule(ctx, "rule", time.Now().UTC()); !errors.Is(err, storage.ErrSyncControlConflict) {
		t.Fatalf("archived in-flight rule: %v", err)
	}
	close(resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if target.created != 1 {
		t.Fatalf("late fake call count: %d", target.created)
	}
	if err = store.FinishRun(ctx, "run", *job, "succeeded", time.Now().UTC(), storage.Run{}); err != nil {
		t.Fatal(err)
	}
	if status, e := store.RuleControlStatus(ctx, "rule"); e != nil || status != "paused" {
		t.Fatalf("status after returned call: %s %v", status, e)
	}
}
