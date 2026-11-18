package calendar

import (
	"context"
	"errors"
	"testing"
	"time"
)

type testDispatchGuard struct {
	released   int
	releaseErr error
}

func (g *testDispatchGuard) BeginWriteDispatch(context.Context, []string) (func(context.Context) error, error) {
	return func(ctx context.Context) error {
		if ctx.Err() != nil {
			panic("release context cancelled")
		}
		g.released++
		return g.releaseErr
	}, nil
}

func TestGuardedWriteNoDispatchContract(t *testing.T) {
	original := errors.New("validation")
	for _, tc := range []struct {
		name         string
		callErr      error
		releaseErr   error
		wantRelease  int
		wantOriginal bool
	}{
		{"preflight", DefinitiveNoDispatch(original), nil, 1, true},
		{"transport unknown", errors.New("timeout"), nil, 0, false},
		{"release failed", DefinitiveNoDispatch(original), errors.New("storage unavailable"), 1, false},
		{"composite child marker", MaskNoDispatch(DefinitiveNoDispatch(original)), nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := &testDispatchGuard{releaseErr: tc.releaseErr}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := GuardedWrite(ctx, guard, []string{"test"}, func() (bool, error) { return false, tc.callErr }, func(v bool) bool { return v })
			if guard.released != tc.wantRelease || errors.Is(err, original) != tc.wantOriginal {
				t.Fatalf("released=%d err=%v", guard.released, err)
			}
			if !tc.wantOriginal {
				var api *APIError
				if !errors.As(err, &api) || api.Code != ErrorMutationOutcomeUnknown {
					t.Fatalf("expected unknown: %v", err)
				}
			}
		})
	}
}

type testGatewayReservation struct {
	deadline time.Time
	cleanup  error
}

func (r testGatewayReservation) ID() string                                { return "synthetic" }
func (r testGatewayReservation) Authorize(context.Context) error           { return nil }
func (r testGatewayReservation) OutcomeDeadline() time.Time                { return r.deadline }
func (r testGatewayReservation) Complete(context.Context, any, bool) error { return r.cleanup }

type testGatewayGuard struct {
	testDispatchGuard
	reservation testGatewayReservation
}

func (g *testGatewayGuard) BeginGatewayWrite(context.Context, []string, GatewayWriteScope) (GatewayWriteReservation, error) {
	return g.reservation, nil
}

func TestTrackedGatewayDispatchUsesReservationDeadlineAndReportsRetainedFence(t *testing.T) {
	ctx, _ := EnableGatewayWriteTracking(t.Context())
	DescribeGatewayWrite(ctx, nil, GatewayWriteScope{Kind: "create", CalendarID: "fixture"})
	guard := &testGatewayGuard{reservation: testGatewayReservation{
		deadline: time.Now().Add(time.Minute), cleanup: errors.New("storage unavailable"),
	}}
	_, err := GuardedWriteContext(ctx, guard, []string{"fixture"}, func(call context.Context) (bool, error) {
		if got, ok := call.Deadline(); !ok || !got.Equal(guard.reservation.deadline) {
			t.Fatalf("provider deadline = %v (present=%v)", got, ok)
		}
		return false, DefinitiveNoDispatch(errors.New("preflight rejected"))
	}, func(v bool) bool { return v })
	var api *APIError
	if !errors.As(err, &api) || api.Code != ErrorConflict || api.Retryable || !errors.Is(err, guard.reservation.cleanup) {
		t.Fatalf("retained reservation error = %v", err)
	}
}
