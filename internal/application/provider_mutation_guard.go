package application

import (
	"context"
	"errors"
	"log"
	"sort"

	"calendar-mcp/internal/calendar"
)

const mutationPendingWarning = "Calendar data will refresh after pending reconciliation."

// WithProviderMutationGuard enables a durable fence before provider mutations.
// A nil guard preserves the existing behavior. Runtime must not enable a
// cache-backed mode until every provider write path uses the same durable guard.
func WithProviderMutationGuard(guard calendar.ProviderMutationGuard) Option {
	return func(s *Service) { s.providerMutationGuard = guard }
}

func (s *Service) beginProviderMutation(ctx context.Context, ids ...string) (calendar.ProviderMutationIntent, error) {
	if s.providerMutationGuard == nil {
		return nil, nil
	}
	sort.Strings(ids)
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, errors.New("calendar mutation protection unavailable")
		}
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	ticket, err := s.providerMutationGuard.BeginProviderMutation(ctx, unique)
	if err != nil {
		return nil, calendar.NewProviderMutationProtectionError(err)
	}
	if ticket == nil {
		return nil, errors.New("calendar mutation protection unavailable")
	}
	return ticket, nil
}

func unknownProviderMutation() error {
	return &calendar.APIError{Code: calendar.ErrorPartialFailure, Message: calendar.ErrProviderMutationOutcomeUnknown.Error(), Cause: calendar.ErrProviderMutationOutcomeUnknown}
}

// finishProviderMutation never turns a successful remote write into an error:
// clients could otherwise retry a create which already happened. An error or
// cancelled context leaves the durable ticket unresolved. The guard's Resolve
// contract is responsible for invalidation and scheduling a clean refresh.
func finishProviderMutation(ctx context.Context, ticket calendar.ProviderMutationIntent, complete bool) bool {
	if ticket == nil {
		return true
	}
	if !complete || ctx.Err() != nil {
		log.Print("calendar provider mutation reconciliation pending")
		return false
	}
	if err := ticket.ResolveProviderMutation(ctx); err != nil {
		log.Print("calendar provider mutation reconciliation pending")
		return false
	}
	return true
}

func hasReconciliationWarning(warnings []string) bool {
	for _, warning := range warnings {
		if warning == reconciliationWarning {
			return true
		}
	}
	return false
}
