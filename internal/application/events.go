package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/googleapi"

	"calendar-mcp/internal/calendar"
)

type Service struct {
	registry *calendar.Registry
}

func New(registry *calendar.Registry) *Service {
	return &Service{registry: registry}
}

func (s *Service) Capabilities(ctx context.Context, calendarID string) (calendar.CalendarCapabilities, error) {
	provider, rawCalendarID, err := s.registry.Resolve(calendarID)
	if err != nil {
		return calendar.CalendarCapabilities{}, invalidArgument(err.Error())
	}
	capabilityProvider, ok := provider.(calendar.CapabilityProvider)
	if !ok {
		return calendar.CalendarCapabilities{}, unsupported(provider.Name(), calendarID, "provider does not expose V2 capabilities")
	}
	capabilities, err := capabilityProvider.Capabilities(ctx, rawCalendarID)
	if err != nil {
		return calendar.CalendarCapabilities{}, providerFailure(provider.Name(), calendarID, err)
	}
	capabilities.Provider = provider.Name()
	capabilities.CalendarID = calendarID
	return capabilities, nil
}

func (s *Service) ListEvents(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if err := validateRange(request.Start, request.End); err != nil {
		return calendar.Page[calendar.EventV2]{}, err
	}
	if request.View == "" {
		request.View = calendar.RecurrenceExpanded
	}
	if request.View != calendar.RecurrenceExpanded && request.View != calendar.RecurrenceSeries && request.View != calendar.RecurrenceBoth {
		return calendar.Page[calendar.EventV2]{}, invalidArgument("view must be expanded, series, or both")
	}
	if request.CalendarID == "" {
		return s.listEventsFanOut(ctx, request), nil
	}
	provider, rawCalendarID, err := s.registry.Resolve(request.CalendarID)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, invalidArgument(err.Error())
	}
	v2, ok := provider.(calendar.EventProviderV2)
	if !ok {
		return calendar.Page[calendar.EventV2]{}, unsupported(provider.Name(), request.CalendarID, "V2 event reads are not supported")
	}
	providerRequest := request
	providerRequest.CalendarID = rawCalendarID
	page, err := v2.ListEventsV2(ctx, providerRequest)
	if err != nil {
		return calendar.Page[calendar.EventV2]{}, providerFailure(provider.Name(), request.CalendarID, err)
	}
	normalizePage(&page, provider.Name(), request.CalendarID)
	if len(page.Sources) == 0 {
		page.Sources = []calendar.SourceStatus{{Provider: provider.Name(), CalendarID: request.CalendarID, Complete: page.Complete}}
	}
	return page, nil
}

func (s *Service) GetEvent(ctx context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	provider, rawRef, err := s.resolveEventRef(ref)
	if err != nil {
		return nil, err
	}
	v2, ok := provider.(calendar.EventProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), ref.CalendarID, "V2 event reads are not supported")
	}
	event, err := v2.GetEventV2(ctx, rawRef)
	if err != nil {
		return nil, providerFailure(provider.Name(), ref.CalendarID, err)
	}
	normalizeEvent(event, provider.Name(), ref.CalendarID)
	return event, nil
}

func (s *Service) CreateEvent(ctx context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	if request.CalendarID == "" {
		return nil, invalidArgument("calendar_id is required")
	}
	if err := calendar.ValidateEventTimeRangeV2(request.Event.Start, request.Event.End); err != nil {
		return nil, invalidArgument(err.Error())
	}
	if err := calendar.ValidateRecurrence(request.Event.Recurrence); err != nil {
		return nil, &calendar.APIError{Code: calendar.ErrorInvalidRecurrence, Message: err.Error(), Cause: err}
	}
	provider, rawCalendarID, err := s.registry.Resolve(request.CalendarID)
	if err != nil {
		return nil, invalidArgument(err.Error())
	}
	v2, ok := provider.(calendar.EventProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), request.CalendarID, "V2 event creation is not supported")
	}
	if err := s.validateNotifications(ctx, provider, rawCalendarID, request.CalendarID, &request.Notifications); err != nil {
		return nil, err
	}
	providerRequest := request
	providerRequest.CalendarID = rawCalendarID
	event, err := v2.CreateEventV2(ctx, providerRequest)
	if err != nil {
		return nil, providerFailure(provider.Name(), request.CalendarID, err)
	}
	normalizeEvent(event, provider.Name(), request.CalendarID)
	return event, nil
}

func (s *Service) UpdateEvent(ctx context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if err := validateMutationScope(request.Scope, request.EffectiveFrom); err != nil {
		return nil, err
	}
	prefixedCalendarID := request.Ref.CalendarID
	provider, rawRef, err := s.resolveEventRef(request.Ref)
	if err != nil {
		return nil, err
	}
	v2, ok := provider.(calendar.EventProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), request.Ref.CalendarID, "V2 event updates are not supported")
	}
	if err := s.validateScopeAndNotifications(ctx, provider, rawRef.CalendarID, request.Ref.CalendarID, request.Scope, &request.Notifications); err != nil {
		return nil, err
	}
	request.Ref = rawRef
	if request.Scope == calendar.ScopeFollowing {
		return s.updateFollowing(ctx, provider, request, prefixedCalendarID)
	}
	result, err := v2.UpdateEventV2(ctx, request)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	normalizeOperation(result, provider.Name(), prefixedCalendarID)
	return result, nil
}

func (s *Service) DeleteEvent(ctx context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if err := validateMutationScope(request.Scope, request.EffectiveFrom); err != nil {
		return nil, err
	}
	prefixedCalendarID := request.Ref.CalendarID
	provider, rawRef, err := s.resolveEventRef(request.Ref)
	if err != nil {
		return nil, err
	}
	v2, ok := provider.(calendar.EventProviderV2)
	if !ok {
		return nil, unsupported(provider.Name(), request.Ref.CalendarID, "V2 event deletion is not supported")
	}
	if err := s.validateScopeAndNotifications(ctx, provider, rawRef.CalendarID, request.Ref.CalendarID, request.Scope, &request.Notifications); err != nil {
		return nil, err
	}
	request.Ref = rawRef
	if request.Scope == calendar.ScopeFollowing {
		return s.deleteFollowing(ctx, provider, request, prefixedCalendarID)
	}
	result, err := v2.DeleteEventV2(ctx, request)
	if err != nil {
		return nil, providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	normalizeOperation(result, provider.Name(), prefixedCalendarID)
	return result, nil
}

func (s *Service) resolveEventRef(ref calendar.EventRef) (calendar.Provider, calendar.EventRef, error) {
	if ref.CalendarID == "" || ref.EventID == "" {
		return nil, calendar.EventRef{}, invalidArgument("calendar_id and event_id are required")
	}
	provider, rawCalendarID, err := s.registry.Resolve(ref.CalendarID)
	if err != nil {
		return nil, calendar.EventRef{}, invalidArgument(err.Error())
	}
	eventProvider, rawEventID := splitPrefix(ref.EventID)
	if eventProvider != "" && eventProvider != provider.Name() {
		return nil, calendar.EventRef{}, invalidArgument(fmt.Sprintf("event provider %q does not match calendar provider %q", eventProvider, provider.Name()))
	}
	return provider, calendar.EventRef{CalendarID: rawCalendarID, EventID: rawEventID}, nil
}

func (s *Service) validateNotifications(ctx context.Context, provider calendar.Provider, rawCalendarID, prefixedCalendarID string, policy *calendar.NotificationPolicy) error {
	if *policy == "" {
		*policy = calendar.NotificationsNone
	}
	capabilityProvider, ok := provider.(calendar.CapabilityProvider)
	if !ok {
		return unsupported(provider.Name(), prefixedCalendarID, "notification policy cannot be verified")
	}
	capabilities, err := capabilityProvider.Capabilities(ctx, rawCalendarID)
	if err != nil {
		return providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	if !capabilities.SupportsNotifications(*policy) {
		return unsupported(provider.Name(), prefixedCalendarID, fmt.Sprintf("notification policy %q is not supported", *policy))
	}
	return nil
}

func (s *Service) validateScopeAndNotifications(ctx context.Context, provider calendar.Provider, rawCalendarID, prefixedCalendarID string, scope calendar.MutationScope, policy *calendar.NotificationPolicy) error {
	capabilityProvider, ok := provider.(calendar.CapabilityProvider)
	if !ok {
		return unsupported(provider.Name(), prefixedCalendarID, "mutation capabilities cannot be verified")
	}
	capabilities, err := capabilityProvider.Capabilities(ctx, rawCalendarID)
	if err != nil {
		return providerFailure(provider.Name(), prefixedCalendarID, err)
	}
	if !capabilities.SupportsScope(scope) {
		return unsupported(provider.Name(), prefixedCalendarID, fmt.Sprintf("mutation scope %q is not supported", scope))
	}
	if *policy == "" {
		*policy = calendar.NotificationsNone
	}
	if !capabilities.SupportsNotifications(*policy) {
		return unsupported(provider.Name(), prefixedCalendarID, fmt.Sprintf("notification policy %q is not supported", *policy))
	}
	return nil
}

func validateRange(start, end time.Time) error {
	if start.IsZero() || end.IsZero() {
		return invalidArgument("start and end are required")
	}
	if !end.After(start) {
		return invalidArgument("end must be after start")
	}
	return nil
}

func validateMutationScope(scope calendar.MutationScope, effectiveFrom *calendar.EventTime) error {
	if scope != calendar.ScopeSeries && scope != calendar.ScopeSingle && scope != calendar.ScopeFollowing {
		return invalidArgument("scope must be series, single, or following")
	}
	if scope == calendar.ScopeFollowing && effectiveFrom == nil {
		return invalidArgument("effective_from is required for following scope")
	}
	if scope != calendar.ScopeFollowing && effectiveFrom != nil {
		return invalidArgument("effective_from is only valid for following scope")
	}
	if effectiveFrom != nil {
		if err := effectiveFrom.Validate(); err != nil {
			return invalidArgument("invalid effective_from: " + err.Error())
		}
	}
	return nil
}

func normalizePage(page *calendar.Page[calendar.EventV2], provider, calendarID string) {
	for i := range page.Items {
		normalizeEvent(&page.Items[i], provider, calendarID)
	}
}

func normalizeEvent(event *calendar.EventV2, provider, calendarID string) {
	event.Provider = provider
	event.CalendarID = calendarID
	event.ID = ensurePrefix(provider, event.ID)
	if event.RecurringEventID != "" {
		event.RecurringEventID = ensurePrefix(provider, event.RecurringEventID)
	}
}

func normalizeOperation(result *calendar.OperationResult, provider, calendarID string) {
	if result == nil {
		return
	}
	if result.Event != nil {
		normalizeEvent(result.Event, provider, calendarID)
	}
	for i := range result.RelatedEvents {
		normalizeEvent(&result.RelatedEvents[i], provider, calendarID)
	}
}

func ensurePrefix(provider, id string) string {
	if id == "" || strings.HasPrefix(id, provider+":") {
		return id
	}
	return provider + ":" + id
}

func splitPrefix(id string) (string, string) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return "", id
	}
	return parts[0], parts[1]
}

func invalidArgument(message string) *calendar.APIError {
	return calendar.NewAPIError(calendar.ErrorInvalidArgument, message)
}

func unsupported(provider, calendarID, message string) *calendar.APIError {
	return &calendar.APIError{Code: calendar.ErrorUnsupportedCapability, Message: message, Provider: provider, CalendarID: calendarID}
}

func providerFailure(provider, calendarID string, err error) *calendar.APIError {
	var apiErr *calendar.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	var googleErr *googleapi.Error
	if errors.As(err, &googleErr) {
		code, retryable := errorCodeForHTTPStatus(googleErr.Code)
		return &calendar.APIError{Code: code, Message: googleErr.Message, Provider: provider, CalendarID: calendarID, Retryable: retryable, Cause: err}
	}
	return &calendar.APIError{Code: calendar.ErrorProviderUnavailable, Message: err.Error(), Provider: provider, CalendarID: calendarID, Retryable: true, Cause: err}
}

func errorCodeForHTTPStatus(status int) (calendar.ErrorCode, bool) {
	switch status {
	case 400:
		return calendar.ErrorInvalidArgument, false
	case 401, 403:
		return calendar.ErrorPermissionDenied, false
	case 404, 410:
		return calendar.ErrorNotFound, false
	case 409, 412:
		return calendar.ErrorConflict, false
	case 429:
		return calendar.ErrorRateLimited, true
	default:
		return calendar.ErrorProviderUnavailable, status >= 500
	}
}
