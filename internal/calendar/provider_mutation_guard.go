package calendar

import (
	"context"
	"errors"
	"log"
)

// ErrProviderMutationOutcomeUnknown indicates an ambiguous provider write
// outcome. Callers must reconcile before considering a retry.
var ErrProviderMutationOutcomeUnknown = errors.New("provider mutation outcome is unknown; reconciliation is required")

// ProviderMutationGuard is an optional pre-dispatch fence. Begin must durably
// record all canonical calendar IDs atomically before returning a ticket. IDs
// are sorted and deduplicated by callers. An error never authorizes dispatch.
// A missing/ambiguous provider outcome leaves the ticket unresolved. This
// calendar-level fence is not an idempotency key: a caller retry could begin a
// separate operation, and this interface does not expose a ticket lookup.
// Reconciliation must be established before using it for uncertain outcomes.
type ProviderMutationGuard interface {
	BeginProviderMutation(ctx context.Context, canonicalCalendarIDs []string) (ProviderMutationIntent, error)
}

// ProviderMutationIntent represents a durable mutation fence. Resolve may be
// called only for a confirmed provider success and must durably invalidate any
// affected snapshots and arrange a later clean sync before they are usable.
// Failure or an ambiguous result must leave the fence safely unresolved.
type ProviderMutationIntent interface {
	ResolveProviderMutation(ctx context.Context) error
}

// ProviderMutationProtectionError prevents private backend details from being
// exposed by legacy entry points while retaining a cause for internal recovery.
type ProviderMutationProtectionError struct{ cause error }

// NewProviderMutationProtectionError keeps pre-dispatch failures safe for
// presentation while allowing internal inspection of the original cause.
func NewProviderMutationProtectionError(cause error) *ProviderMutationProtectionError {
	return &ProviderMutationProtectionError{cause: cause}
}

func (e *ProviderMutationProtectionError) Error() string {
	return "calendar mutation protection unavailable"
}
func (e *ProviderMutationProtectionError) Unwrap() error { return e.cause }

func beginProviderMutation(ctx context.Context, guard ProviderMutationGuard, canonicalCalendarID string) (ProviderMutationIntent, error) {
	if guard == nil {
		return nil, nil
	}
	intent, err := guard.BeginProviderMutation(ctx, []string{canonicalCalendarID})
	if err != nil {
		return nil, NewProviderMutationProtectionError(err)
	}
	if intent == nil {
		return nil, NewProviderMutationProtectionError(errors.New("provider mutation fence unavailable"))
	}
	return intent, nil
}

// resolveProviderMutation preserves the confirmed provider success. The fence
// remains unresolved when local reconciliation fails. The observer receives no
// identifiers or errors; a static log message is used when no observer is set.
func (r *Registry) resolveProviderMutation(ctx context.Context, intent ProviderMutationIntent) {
	if intent == nil {
		return
	}
	if err := intent.ResolveProviderMutation(ctx); err != nil {
		if r.mutationGuardResolveFailure != nil {
			r.mutationGuardResolveFailure()
		} else {
			log.Print("calendar provider mutation succeeded; local reconciliation remains pending")
		}
	}
}

// providerMutationDispatchError leaves the durable fence pending and reports an
// unknown outcome after guarded dispatch. No provider-independent error class
// is assumed to prove a remote write did not happen.
func providerMutationDispatchError(intent ProviderMutationIntent, err error) error {
	if intent != nil {
		return ErrProviderMutationOutcomeUnknown
	}
	return err
}
