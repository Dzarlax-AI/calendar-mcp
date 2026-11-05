package syncengine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

func TestFailureDiagnosticsNeverExposeProviderContent(t *testing.T) {
	secret := "private-title email@example.com token=secret calendar-id"
	cases := []struct {
		name, stage, want string
		err               error
	}{
		{"google auth", "source_read", "sync_source_auth", &googleapi.Error{Code: 401, Message: secret}},
		{"google permission", "source_read", "sync_source_permission", fmt.Errorf("details %s: %w", secret, &googleapi.Error{Code: 403, Message: secret})},
		{"google missing", "source_read", "sync_source_not_found", &googleapi.Error{Code: 404, Message: secret}},
		{"google quota", "source_read", "sync_source_rate_limit", &googleapi.Error{Code: 429, Message: secret}},
		{"google server", "source_read", "sync_source_unavailable", &googleapi.Error{Code: 503, Message: secret}},
		{"oauth auth", "provider_setup", "sync_provider_auth", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 401}, Body: []byte(secret)}},
		{"oauth invalid grant", "source_read", "sync_source_auth", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_grant", ErrorDescription: secret}},
		{"oauth other 400", "source_read", "sync_source_read", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_request", ErrorDescription: secret}},
		{"oauth invalid grant without response", "provider_setup", "sync_provider_auth", &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: secret}},
		{"missing series", "reconciliation", "sync_missing_series_mapping", fmt.Errorf("%w: %s", ErrMissingSeriesMapping, secret)},
		{"unsafe recurring sync", "safe_recurrence_preflight", "sync_unsafe_recurring", fmt.Errorf("%w: %s", ErrUnsafeRecurringSync, secret)},
		{"unknown source", "source_read", "sync_source_read", errors.New(secret)},
		{"unknown stage", secret, "sync_failed", errors.New(secret)},
		{"canceled", "source_read", "sync_interrupted", context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &Failure{Stage: tc.stage, Cause: tc.err}
			code := FailureCode(err)
			message := FailureMessage(code)
			if code != tc.want {
				t.Fatalf("code=%q want=%q", code, tc.want)
			}
			if strings.Contains(code, secret) || strings.Contains(message, secret) || strings.Contains(message, "example.com") {
				t.Fatalf("private data in diagnostics: %q %q", code, message)
			}
			if !errors.Is(err, tc.err) {
				t.Fatal("error cause was not preserved")
			}
		})
	}
	if got := FailureMessage(secret); strings.Contains(got, secret) {
		t.Fatal("unknown code was rendered")
	}
}

func TestUnsafeRecurringSyncMessageExplainsSafeAlternative(t *testing.T) {
	message := FailureMessage("sync_unsafe_recurring")
	normalized := strings.ToLower(message)
	if !strings.Contains(normalized, "recurring") || !strings.Contains(normalized, "cached") || strings.Contains(normalized, "logs") {
		t.Fatalf("unhelpful unsafe-recurring diagnostic: %q", message)
	}
}

func TestFailureCodePrioritizesJoinedLeaseFailureOverCancellation(t *testing.T) {
	err := errors.Join(&Failure{Stage: "source_read", Cause: context.Canceled}, &Failure{Stage: "job_lease", Cause: errors.New("lease details")})
	if got := FailureCode(err); got != "sync_job_lease" {
		t.Fatalf("code=%q", got)
	}
	if got := FailureCode(&Failure{Stage: "source_read", Cause: context.Canceled}); got != "sync_interrupted" {
		t.Fatalf("cancellation=%q", got)
	}
}
