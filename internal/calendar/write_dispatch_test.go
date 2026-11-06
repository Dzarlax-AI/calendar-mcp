package calendar

import (
	"context"
	"errors"
	"testing"
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
