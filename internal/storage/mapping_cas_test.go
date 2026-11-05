package storage

import (
	"errors"
	"testing"
	"time"
)

func TestMappingCASProtectsPendingAndStaleWriters(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	if err := s.UpsertCalendar(ctx, Calendar{ID: "target", ConnectionID: "connection", ProviderCalendarID: "target", Name: "Target", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRule(ctx, Rule{ID: "rule", SourceCalendarID: "calendar", TargetCalendarID: "target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	pending := Mapping{ID: "mapping", RuleID: "rule", ObjectKind: "event", SourceEventID: "source", TargetEventID: "pending:mapping", ReconciliationState: "pending_create", LastSeenAt: now}
	if ok, err := s.InsertPendingCreateMapping(ctx, pending); err != nil || ok == "" {
		t.Fatalf("insert: %v %v", ok, err)
	}
	list, err := s.ListMappings(ctx, "rule")
	if err != nil || len(list) != 1 || list[0].Version != 1 {
		t.Fatalf("version: %+v %v", list, err)
	}
	stale := list[0]
	old := stale
	old.ReconciliationState = "current"
	old.TargetEventID = "target-event"
	if err := s.UpsertMapping(ctx, old); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("legacy upsert overwrote pending: %v", err)
	}
	if err := s.DeleteMapping(ctx, old.ID); err == nil {
		t.Fatal("legacy delete removed pending")
	}
	if err := s.UpdateMappingCAS(ctx, old, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMappingCAS(ctx, old, 1); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	if err := s.DeleteMappingCAS(ctx, old.ID, old.Incarnation, 1); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("stale delete accepted: %v", err)
	}
	list, err = s.ListMappings(ctx, "rule")
	if err != nil || len(list) != 1 || list[0].Version != 2 || list[0].TargetEventID != "target-event" {
		t.Fatalf("current: %+v %v", list, err)
	}
	next := list[0]
	next.ReconciliationState = "pending_update"
	next.LastSeenAt = now.Add(time.Second)
	if err = s.UpdateMappingCAS(ctx, next, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteMapping(ctx, next.ID); err == nil {
		t.Fatal("legacy deleted pending update")
	}
	if err = s.DeleteMappingCAS(ctx, next.ID, next.Incarnation, 3); err != nil {
		t.Fatal(err)
	}
	oldIncarnation := stale.Incarnation
	newIncarnation, err := s.InsertPendingCreateMapping(ctx, pending)
	if err != nil || newIncarnation == "" || newIncarnation == oldIncarnation {
		t.Fatalf("recreate: %v", err)
	}
	if err := s.UpdateMappingCAS(ctx, old, 1); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("ABA update accepted: %v", err)
	}
	if err := s.DeleteMappingCAS(ctx, pending.ID, oldIncarnation, 1); !errors.Is(err, ErrMappingConflict) {
		t.Fatalf("ABA delete accepted: %v", err)
	}
	if err := s.DeleteMappingCAS(ctx, pending.ID, newIncarnation, 1); err != nil {
		t.Fatal(err)
	}

	current := Mapping{ID: "race", RuleID: "rule", SourceEventID: "race-source", TargetEventID: "target-event", ReconciliationState: "current", LastSeenAt: now}
	if err = s.UpsertMapping(ctx, current); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListMappings(ctx, "rule")
	if err != nil || len(list) != 1 {
		t.Fatalf("race mapping: %v", err)
	}
	current = list[0]
	results := make(chan error, 2)
	for _, state := range []string{"pending_update", "pending_delete"} {
		candidate := current
		candidate.ReconciliationState = state
		go func() { results <- s.UpdateMappingCAS(ctx, candidate, 1) }()
	}
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ErrMappingConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS race: %d wins, %d conflicts", wins, conflicts)
	}
	intent := Mapping{ID: "exception", RuleID: "rule", SourceEventID: "exception-source", TargetEventID: "exception-target", ReconciliationState: "pending_update", LastSeenAt: now}
	if ok, err := s.InsertPendingMutationMapping(ctx, intent); err != nil || ok == "" {
		t.Fatalf("insert mutation: %v %v", ok, err)
	}
	if ok, err := s.InsertPendingMutationMapping(ctx, intent); err != nil || ok != "" {
		t.Fatalf("duplicate mutation: %v %v", ok, err)
	}
	if err := s.DeleteMapping(ctx, intent.ID); err == nil {
		t.Fatal("legacy delete removed inserted pending mutation")
	}
}
