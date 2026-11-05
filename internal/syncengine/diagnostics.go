package syncengine

import (
	"context"
	"errors"
	"net/http"

	"calendar-mcp/internal/calendar"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// FailureCode returns a finite code. Provider text and identifiers are never persisted.
func FailureCode(err error) string {
	var recurrence *RecurrenceCompatibilityError
	if errors.As(err, &recurrence) {
		return "recurrence_unsupported"
	}
	if errors.Is(err, ErrMissingSeriesMapping) {
		return "sync_missing_series_mapping"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "sync_interrupted"
	}
	var failure *Failure
	stage := ""
	if errors.As(err, &failure) {
		stage = failure.Stage
	}
	var api *googleapi.Error
	var oauth *oauth2.RetrieveError
	var status int
	if errors.As(err, &api) {
		status = api.Code
	} else if errors.As(err, &oauth) && oauth.Response != nil {
		status = oauth.Response.StatusCode
	}
	var typed *calendar.APIError
	if errors.As(err, &typed) {
		switch typed.Code {
		case calendar.ErrorPermissionDenied:
			status = http.StatusForbidden
		case calendar.ErrorRateLimited:
			status = http.StatusTooManyRequests
		case calendar.ErrorProviderUnavailable:
			status = http.StatusServiceUnavailable
		}
	}
	if stage == "source_read" {
		switch {
		case status == 401:
			return "sync_source_auth"
		case status == 403:
			return "sync_source_permission"
		case status == 404 || status == 410:
			return "sync_source_not_found"
		case status == 429:
			return "sync_source_rate_limit"
		case status >= 500 && status <= 599:
			return "sync_source_unavailable"
		}
	}
	switch {
	case status == 401:
		return "sync_provider_auth"
	case status == 403:
		return "sync_provider_permission"
	case status == 429:
		return "sync_provider_rate_limit"
	case status >= 500 && status <= 599:
		return "sync_provider_unavailable"
	}
	switch stage {
	case "rule_validation":
		return "sync_rule_invalid"
	case "source_selection":
		return "sync_source_selection"
	case "target_selection":
		return "sync_target_selection"
	case "capabilities":
		return "sync_capabilities"
	case "source_read":
		return "sync_source_read"
	case "mappings":
		return "sync_mappings"
	case "recurrence":
		return "sync_recurrence_check"
	case "reconciliation":
		return "sync_reconciliation"
	case "provider_setup":
		return "sync_provider_setup"
	case "job_lease":
		return "sync_job_lease"
	default:
		return "sync_failed"
	}
}

// FailureMessage accepts only known codes; it never renders stored provider text.
func FailureMessage(code string) string {
	switch code {
	case "recurrence_unsupported":
		return "Target calendar cannot preserve this recurrence pattern; the rule remains paused."
	case "sync_missing_series_mapping":
		return "A recurring event exception has no matching series in the sync mapping."
	case "sync_interrupted":
		return "The sync run was interrupted or timed out."
	case "sync_source_auth":
		return "The source calendar provider rejected authentication."
	case "sync_source_permission":
		return "The provider denied access while reading the source calendar."
	case "sync_source_not_found":
		return "The source calendar or a required source event was not found."
	case "sync_source_rate_limit":
		return "The source calendar provider rate-limited the request."
	case "sync_source_unavailable":
		return "The source calendar provider was temporarily unavailable."
	case "sync_provider_auth":
		return "A calendar provider rejected authentication."
	case "sync_provider_permission":
		return "A calendar provider denied access."
	case "sync_provider_rate_limit":
		return "A calendar provider rate-limited the request."
	case "sync_provider_unavailable":
		return "A calendar provider was temporarily unavailable."
	case "sync_rule_invalid":
		return "The sync rule configuration is invalid."
	case "sync_source_selection":
		return "The source calendar could not be selected."
	case "sync_target_selection":
		return "The destination calendar could not be selected."
	case "sync_capabilities":
		return "A calendar does not support the required sync operations."
	case "sync_source_read":
		return "The source calendar could not be read."
	case "sync_mappings":
		return "The sync mappings could not be read."
	case "sync_recurrence_check":
		return "Recurring events could not be checked."
	case "sync_reconciliation":
		return "Events could not be reconciled with the destination calendar."
	case "sync_provider_setup":
		return "The calendar provider connection could not be initialized."
	case "sync_job_lease":
		return "The sync worker could not retain the job lease."
	default:
		return "Run failed; inspect server logs for details."
	}
}
