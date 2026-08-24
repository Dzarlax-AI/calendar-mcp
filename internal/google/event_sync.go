package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"

	"calendar-mcp/internal/calendar"
)

// EventSyncPolicy supplies the bounded defaults appropriate for Google's
// incremental Events.List feed. The coordinator owns enforcement.
func (p *Provider) EventSyncPolicy() calendar.EventSyncPolicy {
	return calendar.EventSyncPolicy{
		PollInterval: time.Minute,
		RetryBase:    5 * time.Second,
		RetryMax:     5 * time.Minute,
		MaxPages:     250,
		MaxResets:    2,
	}
}

// SyncEvents reads exactly one Google Events.List page. Google event IDs are
// provider-local object identities, so this adapter never prefixes them.
func (p *Provider) SyncEvents(ctx context.Context, request calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	if request.CalendarID == "" || p == nil || p.svc == nil {
		return calendar.EventSyncPage{}, syncProtocolError(nil)
	}
	if !validSyncWindow(request.Window) {
		return calendar.EventSyncPage{}, syncProtocolError(nil)
	}

	call := p.svc.Events.List(request.CalendarID).ShowDeleted(true).SingleEvents(true).MaxResults(2500)
	switch request.Mode {
	case calendar.EventSyncReplacement:
		// The initial request establishes the sync token. It intentionally uses
		// the same unbounded parameter set as later sync-token requests; the
		// frozen projection window is applied locally below.
	case calendar.EventSyncIncremental:
		if request.Cursor == "" {
			return calendar.EventSyncPage{}, syncProtocolError(nil)
		}
		// syncToken is deliberately not combined with replacement-only bounds.
		call = call.SyncToken(string(request.Cursor))
	default:
		return calendar.EventSyncPage{}, syncProtocolError(nil)
	}
	if request.PageToken != "" {
		call = call.PageToken(string(request.PageToken))
	}

	response, err := call.Context(ctx).Do()
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusGone {
			// A stale sync token requires replacement, but must not mutate this page.
			return calendar.EventSyncPage{ResetRequired: true}, nil
		}
		return calendar.EventSyncPage{}, classifyGoogleSyncError(err)
	}

	page := calendar.EventSyncPage{}
	for _, item := range response.Items {
		if item == nil || item.Id == "" {
			return calendar.EventSyncPage{}, syncProtocolError(nil)
		}
		if item.Status == "cancelled" {
			page.DeletedEventIDs = append(page.DeletedEventIDs, item.Id)
			continue
		}
		event := fromGoogleEventV2(item, request.CalendarID, response.TimeZone)
		inWindow, windowErr := googleEventInSyncWindow(event, request.Window)
		if windowErr != nil {
			// A single malformed/special Google event must not discard the
			// otherwise valid page or park the entire calendar. Keep the page
			// degraded so the cursor is not advanced past the bad object; the
			// coordinator retries it on the bounded degraded cadence.
			page.Warnings = append(page.Warnings, calendar.EventSyncWarning{
				Code: calendar.EventSyncProtocol, ObjectID: item.Id, ETag: item.Etag,
				Diagnostic: googleEventDiagnostic(item),
			})
			continue
		}
		if !inWindow {
			// The initial feed is unbounded to establish a parameter-compatible
			// token, so replacement pages ignore objects outside the projection.
			// Incremental changes may move a cached item outside it; tombstone it.
			if request.Mode == calendar.EventSyncIncremental {
				page.DeletedEventIDs = append(page.DeletedEventIDs, item.Id)
			}
			continue
		}
		page.Upserts = append(page.Upserts, calendar.EventSyncUpsert{
			Object: calendar.SyncObject{ObjectID: item.Id, ETag: item.Etag},
			Event:  event,
		})
	}

	if response.NextPageToken != "" {
		page.NextPageToken = calendar.EventSyncPageToken(response.NextPageToken)
		return page, nil
	}
	page.Complete = true
	// Google only emits the durable sync token on the final page.
	page.NextCursor = calendar.EventSyncCursor(response.NextSyncToken)
	return page, nil
}

// RepairEventSyncObject refetches one malformed event without replaying the
// whole calendar feed. A provider 404/410 is a confirmed deletion; malformed
// data remains quarantined with the representation ETag observed by Google.
func (p *Provider) RepairEventSyncObject(ctx context.Context, request calendar.EventSyncObjectRepairRequest) (calendar.EventSyncObjectRepairResult, error) {
	if p == nil || p.svc == nil || request.CalendarID == "" || request.Object.ObjectID == "" {
		return calendar.EventSyncObjectRepairResult{}, syncProtocolError(nil)
	}
	item, err := p.svc.Events.Get(request.CalendarID, request.Object.ObjectID).Context(ctx).Do()
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && (apiErr.Code == http.StatusNotFound || apiErr.Code == http.StatusGone) {
			return calendar.EventSyncObjectRepairResult{Object: request.Object, Outcome: calendar.EventSyncObjectProviderDeleted}, nil
		}
		return calendar.EventSyncObjectRepairResult{}, classifyGoogleSyncError(err)
	}
	object := calendar.SyncObject{ObjectID: item.Id, ETag: item.Etag}
	if item.Status == "cancelled" {
		return calendar.EventSyncObjectRepairResult{Object: object, Outcome: calendar.EventSyncObjectProviderDeleted}, nil
	}
	event := fromGoogleEventV2(item, request.CalendarID, "")
	inWindow, windowErr := googleEventInSyncWindow(event, request.Window)
	if windowErr != nil {
		return calendar.EventSyncObjectRepairResult{Object: object, Outcome: calendar.EventSyncObjectStillQuarantined, Warning: &calendar.EventSyncWarning{Code: calendar.EventSyncProtocol, ObjectID: item.Id, ETag: item.Etag, Diagnostic: googleEventDiagnostic(item)}}, nil
	}
	if !inWindow {
		return calendar.EventSyncObjectRepairResult{Object: object, Outcome: calendar.EventSyncObjectAbsentFromProjection}, nil
	}
	return calendar.EventSyncObjectRepairResult{Object: object, Outcome: calendar.EventSyncObjectReplaceMembership, Upserts: []calendar.EventSyncUpsert{{Object: object, Event: event}}}, nil
}

func googleEventDiagnostic(item *gcal.Event) *calendar.EventSyncDiagnostic {
	if item == nil {
		return nil
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	return &calendar.EventSyncDiagnostic{ContentType: "application/json", RawPayload: payload}
}

func googleEventInSyncWindow(event calendar.EventV2, window calendar.EventSyncWindow) (bool, error) {
	if !validSyncWindow(window) {
		return false, errors.New("invalid sync window")
	}
	start, err := event.Start.Instant()
	if err != nil {
		return false, err
	}
	end, err := event.End.Instant()
	if err != nil {
		return false, err
	}
	if !end.After(start) {
		return false, errors.New("invalid event time range")
	}
	return start.Before(window.End) && end.After(window.Start), nil
}

func validSyncWindow(window calendar.EventSyncWindow) bool {
	return !window.Start.IsZero() && !window.End.IsZero() && window.End.After(window.Start)
}

func classifyGoogleSyncError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		var retrieveErr *oauth2.RetrieveError
		if errors.As(err, &retrieveErr) {
			status := 0
			if retrieveErr.Response != nil {
				status = retrieveErr.Response.StatusCode
			}
			class := calendar.EventSyncTransient
			if status == http.StatusUnauthorized || retrieveErr.ErrorCode == "invalid_grant" {
				class = calendar.EventSyncAuth
			}
			return &calendar.EventSyncError{Class: class, ProviderStatus: status, ProviderReason: strings.TrimSpace(retrieveErr.ErrorCode), Cause: err}
		}
		return &calendar.EventSyncError{Class: calendar.EventSyncTransient, Cause: err}
	}
	class := calendar.EventSyncProtocol
	reason := ""
	if len(apiErr.Errors) > 0 {
		// Google supplies a bounded machine-readable reason alongside the HTTP
		// status. Keep only that allowlisted value; never expose response bodies,
		// URLs, cursors, or credentials in logs.
		reason = strings.TrimSpace(apiErr.Errors[0].Reason)
	}
	switch {
	case apiErr.Code == http.StatusUnauthorized:
		class = calendar.EventSyncAuth
	case apiErr.Code == http.StatusForbidden && googleSyncRateLimited(apiErr):
		class = calendar.EventSyncRateLimited
	case apiErr.Code == http.StatusForbidden:
		class = calendar.EventSyncPermission
	case apiErr.Code == http.StatusTooManyRequests:
		class = calendar.EventSyncRateLimited
	case apiErr.Code >= 500 && apiErr.Code <= 599:
		class = calendar.EventSyncTransient
	}
	return &calendar.EventSyncError{Class: class, RetryAfter: googleRetryAfter(apiErr.Header), ProviderStatus: apiErr.Code, ProviderReason: reason, Cause: err}
}

func googleSyncRateLimited(apiErr *googleapi.Error) bool {
	if apiErr == nil {
		return false
	}
	for _, item := range apiErr.Errors {
		switch strings.ToLower(item.Reason) {
		case "ratelimitexceeded", "userratelimitexceeded", "quotaexceeded", "dailylimitexceeded", "calendarusagelimitsexceeded":
			return true
		}
	}
	return false
}

func syncProtocolError(cause error) error {
	return &calendar.EventSyncError{Class: calendar.EventSyncProtocol, Cause: cause}
}

func googleRetryAfter(header http.Header) time.Duration {
	if header == nil {
		return 0
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(retryAt))
	}
	return 0
}
