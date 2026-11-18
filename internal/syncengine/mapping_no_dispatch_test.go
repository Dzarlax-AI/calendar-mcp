package syncengine

import (
	"context"
	"errors"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type noDispatchRestoreFence struct {
	mappings *fakeMappings
	err      error
}

func (f noDispatchRestoreFence) CheckProviderDispatch(context.Context) error { return nil }
func (f noDispatchRestoreFence) RestoreMappingAfterNoDispatch(ctx context.Context, previous, pending storage.Mapping) error {
	if f.err != nil {
		return f.err
	}
	previous.Version = pending.Version
	return f.mappings.UpdateMappingCAS(ctx, previous, pending.Version)
}

func TestNoDispatchUpdateRestoration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      error
		restoreErr error
		restored   bool
	}{
		{name: "definitive", cause: calendar.DefinitiveNoDispatch(errors.New("private provider text")), restored: true},
		{name: "unknown", cause: errors.New("unknown outcome")},
		{name: "lost lease", cause: calendar.DefinitiveNoDispatch(errors.New("private provider text")), restoreErr: storage.ErrJobLeaseLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := storage.Mapping{ID: "m", RuleID: "r", SourceEventID: "s", TargetEventID: "t", ContentHash: "old", TargetETag: "etag", ReconciliationState: "current", Incarnation: "inc", Version: 1}
			pending := previous
			pending.Version = 2
			pending.ReconciliationState = "pending_update"
			mappings := &fakeMappings{values: map[string]storage.Mapping{"m": pending}}
			calls := 0
			e := New(nil, mappings).WithDispatchFence(noDispatchRestoreFence{mappings, tc.restoreErr}).WithNoDispatchObserver(func() { calls++ })
			err := e.reconcileNoDispatchUpdate(t.Context(), previous, pending, tc.cause)
			if err == nil {
				t.Fatal("missing error")
			}
			got := mappings.values["m"]
			if tc.restored {
				if calls != 1 || got.ReconciliationState != "current" || got.Version != 3 || got.ContentHash != "old" || got.TargetETag != "etag" || FailureCode(err) != "sync_target_pre_dispatch" {
					t.Fatalf("restoration: %+v calls %d code %s", got, calls, FailureCode(err))
				}
			} else if calls != 0 || got.ReconciliationState != "pending_update" || got.Version != 2 {
				t.Fatalf("unsafe restoration: %+v calls %d", got, calls)
			}
		})
	}
}
