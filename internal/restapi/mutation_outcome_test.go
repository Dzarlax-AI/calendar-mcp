package restapi

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
		for _, legacy := range []bool{false, true} {
			r := httptest.NewRecorder()
			if legacy {
				writeLegacyMutationError(r, err)
			} else {
				writeAPIError(r, err)
			}
			body := r.Body.String()
			if r.Code != http.StatusConflict || !strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "Check the calendar") || !strings.Contains(body, `"retryable":false`) || strings.Contains(body, "private-") {
				t.Fatalf("legacy=%v status=%d body=%s", legacy, r.Code, body)
			}
		}
	}
}

func TestProtectionErrorTakesPriorityOverUnknownOutcome(t *testing.T) {
	guarded := calendar.NewProviderMutationProtectionError(fmt.Errorf("private-secret: %w", calendar.ErrProviderMutationOutcomeUnknown))
	for _, value := range []error{guarded, fmt.Errorf("private-wrapper: %w", guarded)} {
		w := httptest.NewRecorder()
		writeAPIError(w, value)
		body := w.Body.String()
		if w.Code != http.StatusBadGateway || strings.Contains(body, "private-") || strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "calendar mutation protection unavailable") {
			t.Fatalf("status=%d body=%s", w.Code, body)
		}
	}
}
