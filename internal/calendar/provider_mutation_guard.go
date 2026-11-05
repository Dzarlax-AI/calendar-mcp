package calendar

import (
	"errors"
)

// ErrProviderMutationOutcomeUnknown indicates an ambiguous provider write
// outcome. Callers must reconcile before considering a retry.
var ErrProviderMutationOutcomeUnknown = errors.New("provider mutation outcome is unknown; reconciliation is required")

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
