package calendar

import (
	"context"
	"errors"
	"time"
)

// WriteDispatchGuard reserves a shared, durable fence before provider writes.
// A release is permitted only after a conclusive result.
type WriteDispatchGuard interface {
	BeginWriteDispatch(context.Context, []string) (func(context.Context) error, error)
}

var ErrWriteDispatchBlocked = errors.New("calendar write is blocked by an active or unresolved operation")

// noDispatchError is valid only when the entire operation has made no provider
// mutation request. It must not cross a composite-operation boundary.
type noDispatchError struct{ err error }

func (e *noDispatchError) Error() string { return e.err.Error() }
func (e *noDispatchError) Unwrap() error { return e.err }

// DefinitiveNoDispatch marks an error known to precede every mutation request.
func DefinitiveNoDispatch(err error) error {
	if err == nil {
		return nil
	}
	return &noDispatchError{err: err}
}

// MaskNoDispatch prevents a child operation's marker from asserting anything
// about earlier writes in a composite operation.
func MaskNoDispatch(err error) error {
	var marker *noDispatchError
	if errors.As(err, &marker) {
		return &APIError{Code: ErrorMutationOutcomeUnknown, Message: MutationOutcomeUnknownMessage, Cause: ErrProviderMutationOutcomeUnknown}
	}
	return err
}

// GuardedWrite retains the reservation unless the operation is confirmed or
// explicitly proves that no provider mutation was dispatched.
func GuardedWrite[T any](ctx context.Context, guard WriteDispatchGuard, ids []string, call func() (T, error), confirmed func(T) bool) (T, error) {
	if guard == nil {
		return call()
	}
	var zero T
	release, err := guard.BeginWriteDispatch(ctx, ids)
	if err != nil {
		return zero, ErrWriteDispatchBlocked
	}
	result, err := call()
	marker, noDispatch := err.(*noDispatchError)
	if err != nil && !noDispatch {
		return zero, &APIError{Code: ErrorMutationOutcomeUnknown, Message: MutationOutcomeUnknownMessage, Cause: ErrProviderMutationOutcomeUnknown}
	}
	if err == nil && !confirmed(result) {
		return result, nil
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := release(releaseCtx); err != nil {
		return zero, &APIError{Code: ErrorMutationOutcomeUnknown, Message: MutationOutcomeUnknownMessage, Cause: ErrProviderMutationOutcomeUnknown}
	}
	if marker != nil {
		return zero, marker.err
	}
	return result, nil
}
