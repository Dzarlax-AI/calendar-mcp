package application

import "calendar-mcp/internal/calendar"

func unknownProviderMutation() error {
	return &calendar.APIError{Code: calendar.ErrorPartialFailure, Message: calendar.ErrProviderMutationOutcomeUnknown.Error(), Cause: calendar.ErrProviderMutationOutcomeUnknown}
}
