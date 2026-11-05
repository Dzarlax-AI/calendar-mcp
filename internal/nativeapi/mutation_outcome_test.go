package nativeapi

import (
	"calendar-mcp/internal/calendar"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMutationOutcomeUnknownIsNotRetryable(t *testing.T) {
	for _, err := range []error{calendar.ErrProviderMutationOutcomeUnknown, fmt.Errorf("private-context: %w", calendar.ErrProviderMutationOutcomeUnknown), &calendar.APIError{Code: calendar.ErrorPartialFailure, Message: "private-payload", Cause: calendar.ErrProviderMutationOutcomeUnknown}} {
		r := httptest.NewRecorder()
		writeApplicationError(r, err)
		body := r.Body.String()
		if r.Code != http.StatusConflict || !strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "Check the calendar") || !strings.Contains(body, `"retryable":false`) || strings.Contains(body, "private-") {
			t.Fatalf("status=%d body=%s", r.Code, body)
		}
	}
}

func TestGuardPredispatchErrorTakesPriorityOverUnknownOutcome(t *testing.T) {
	guarded := calendar.NewProviderMutationProtectionError(fmt.Errorf("private-secret: %w", calendar.ErrProviderMutationOutcomeUnknown))
	for _, value := range []error{guarded, fmt.Errorf("private-wrapper: %w", guarded)} {
		w := httptest.NewRecorder()
		writeApplicationError(w, value)
		body := w.Body.String()
		if w.Code != http.StatusBadGateway || strings.Contains(body, "private-") || strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "calendar mutation protection unavailable") {
			t.Fatalf("status=%d body=%s", w.Code, body)
		}
	}
}
