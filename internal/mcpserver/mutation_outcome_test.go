package mcpserver

import (
	"calendar-mcp/internal/calendar"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMutationOutcomeUnknownIsNotRetryable(t *testing.T) {
	for _, err := range []error{calendar.ErrProviderMutationOutcomeUnknown, fmt.Errorf("private-context: %w", calendar.ErrProviderMutationOutcomeUnknown), &calendar.APIError{Code: calendar.ErrorPartialFailure, Message: "private-payload", Cause: calendar.ErrProviderMutationOutcomeUnknown}} {
		result := v2Error(err)
		data, _ := json.Marshal(result)
		body := string(data)
		if !result.IsError || !strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "Check the calendar") || !strings.Contains(body, `"retryable":false`) || strings.Contains(body, "private-") {
			t.Fatalf("v2 body=%s", body)
		}
		result = legacyMutationError(err)
		data, _ = json.Marshal(result)
		body = string(data)
		if !result.IsError || !strings.Contains(body, "Check the calendar") || !strings.Contains(body, `"retryable":false`) || strings.Contains(body, "private-") {
			t.Fatalf("legacy body=%s", body)
		}
	}
}

func TestProtectionErrorTakesPriorityOverUnknownOutcome(t *testing.T) {
	guarded := calendar.NewProviderMutationProtectionError(fmt.Errorf("private-secret: %w", calendar.ErrProviderMutationOutcomeUnknown))
	for _, value := range []error{guarded, fmt.Errorf("private-wrapper: %w", guarded)} {
		for _, legacy := range []bool{false, true} {
			result := v2Error(value)
			if legacy {
				result = legacyMutationError(value)
			}
			data, _ := json.Marshal(result)
			body := string(data)
			if !result.IsError || strings.Contains(body, "private-") || strings.Contains(body, string(calendar.ErrorMutationOutcomeUnknown)) || !strings.Contains(body, "calendar mutation protection unavailable") {
				t.Fatalf("legacy=%v body=%s", legacy, body)
			}
		}
	}
}
