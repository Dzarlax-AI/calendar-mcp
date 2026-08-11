package calendar

import "fmt"

type ErrorCode string

const (
	ErrorInvalidArgument       ErrorCode = "invalid_argument"
	ErrorInvalidRecurrence     ErrorCode = "invalid_recurrence"
	ErrorUnsupportedCapability ErrorCode = "unsupported_capability"
	ErrorNotFound              ErrorCode = "not_found"
	ErrorPermissionDenied      ErrorCode = "permission_denied"
	ErrorConflict              ErrorCode = "conflict"
	ErrorRateLimited           ErrorCode = "rate_limited"
	ErrorProviderUnavailable   ErrorCode = "provider_unavailable"
	ErrorPartialFailure        ErrorCode = "partial_failure"
)

type APIError struct {
	Code       ErrorCode `json:"code"`
	Message    string    `json:"message"`
	Provider   string    `json:"provider,omitempty"`
	CalendarID string    `json:"calendar_id,omitempty"`
	EventID    string    `json:"event_id,omitempty"`
	Retryable  bool      `json:"retryable,omitempty"`
	Cause      error     `json:"-"`
}

func (e *APIError) Error() string {
	if e.Provider == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s (%s): %s", e.Code, e.Provider, e.Message)
}

func (e *APIError) Unwrap() error { return e.Cause }

func NewAPIError(code ErrorCode, message string) *APIError {
	return &APIError{Code: code, Message: message}
}
