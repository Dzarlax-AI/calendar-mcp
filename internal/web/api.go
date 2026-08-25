package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

// The browser API is deliberately a narrower contract than the external V2
// API. In particular, it never serializes attendee, conferencing, reminder,
// attachment, credential, or MCP key data.
const (
	maxUIEventPages        = 100
	maxUIRequestBodyBytes  = 64 << 10
	maxUIEventRange        = 93 * 24 * time.Hour
	uiEventSourceTimeout   = 20 * time.Second
	maxDiagnosticsPageSize = 100
)

type uiError struct {
	Code      calendar.ErrorCode `json:"code"`
	Message   string             `json:"message"`
	Retryable bool               `json:"retryable,omitempty"`
	// Origin lets the browser distinguish an application JSON error from a
	// session redirect or proxy response that did not reach this service.
	Origin string `json:"origin"`
}

const uiErrorOriginApplication = "application"

type uiEvent struct {
	ID               string              `json:"id"`
	CalendarID       string              `json:"calendar_id"`
	Provider         string              `json:"provider"`
	ETag             string              `json:"etag,omitempty"`
	Title            string              `json:"title,omitempty"`
	Description      string              `json:"description,omitempty"`
	Location         string              `json:"location,omitempty"`
	Status           string              `json:"status,omitempty"`
	Start            calendar.EventTime  `json:"start"`
	End              calendar.EventTime  `json:"end"`
	OriginalStart    *calendar.EventTime `json:"original_start,omitempty"`
	RecurringEventID string              `json:"recurring_event_id,omitempty"`
	InstanceKind     string              `json:"instance_kind,omitempty"`
	Recurrence       []string            `json:"recurrence,omitempty"`
	Visibility       string              `json:"visibility,omitempty"`
	Transparency     string              `json:"transparency,omitempty"`
	ColorID          string              `json:"color_id,omitempty"`
	Created          *time.Time          `json:"created,omitempty"`
	Updated          *time.Time          `json:"updated,omitempty"`
	ReadOnly         bool                `json:"read_only,omitempty"`
	Warnings         []string            `json:"warnings,omitempty"`
}

type uiSourceStatus struct {
	Provider      string     `json:"provider"`
	CalendarID    string     `json:"calendar_id,omitempty"`
	Complete      bool       `json:"complete"`
	Error         *uiError   `json:"error,omitempty"`
	Status        string     `json:"status"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	Stale         bool       `json:"stale"`
	ErrorCode     *string    `json:"error_code"`
}

type uiEventsResponse struct {
	Items    []uiEvent        `json:"items"`
	Complete bool             `json:"complete"`
	Sources  []uiSourceStatus `json:"sources"`
}

type uiRawSyncArtifact struct {
	CalendarID     string    `json:"calendar_id"`
	ObjectID       string    `json:"object_id"`
	ETag           string    `json:"etag,omitempty"`
	PayloadBase64  string    `json:"payload_base64"`
	PayloadSHA256  string    `json:"payload_sha256"`
	ContentType    string    `json:"content_type,omitempty"`
	ProviderStatus int       `json:"provider_status,omitempty"`
	ProviderReason string    `json:"provider_reason,omitempty"`
	Truncated      bool      `json:"truncated"`
	CapturedAt     time.Time `json:"captured_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type rawSyncArtifactRequest struct {
	CalendarID string `json:"calendar_id"`
	ObjectID   string `json:"object_id"`
}

type diagnosticsQuarantineRequest struct {
	CalendarID   string `json:"calendar_id"`
	ObjectID     string `json:"object_id"`
	ExpectedETag string `json:"expected_etag"`
}

type uiDiagnosticsQuarantine struct {
	CalendarID     string                     `json:"calendar_id"`
	ObjectID       string                     `json:"object_id"`
	ETag           string                     `json:"etag,omitempty"`
	ErrorCode      string                     `json:"error_code"`
	CalendarName   string                     `json:"calendar_name"`
	Provider       string                     `json:"provider"`
	SyncStatus     string                     `json:"sync_status"`
	LastErrorCode  string                     `json:"last_error_code,omitempty"`
	FirstSeenAt    time.Time                  `json:"first_seen_at"`
	LastSeenAt     time.Time                  `json:"last_seen_at"`
	NextRepairAt   time.Time                  `json:"next_repair_at"`
	RepairAttempts int                        `json:"repair_attempts"`
	LastSuccessAt  *time.Time                 `json:"last_success_at,omitempty"`
	Artifact       *uiRawSyncArtifactMetadata `json:"artifact,omitempty"`
}

type uiRawSyncArtifactMetadata struct {
	ETag           string    `json:"etag,omitempty"`
	PayloadSHA256  string    `json:"payload_sha256"`
	ContentType    string    `json:"content_type,omitempty"`
	ProviderReason string    `json:"provider_reason,omitempty"`
	ProviderStatus int       `json:"provider_status,omitempty"`
	Truncated      bool      `json:"truncated"`
	CapturedAt     time.Time `json:"captured_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type uiDiagnosticsQuarantineResponse struct {
	Items  []uiDiagnosticsQuarantine `json:"items"`
	Offset int                       `json:"offset"`
	Limit  int                       `json:"limit"`
}

type uiDiagnosticsProviderCorrection struct {
	CalendarID   string    `json:"calendar_id"`
	ObjectID     string    `json:"object_id"`
	Outcome      string    `json:"outcome"`
	CorrectedAt  time.Time `json:"corrected_at"`
	CalendarName string    `json:"calendar_name"`
	Provider     string    `json:"provider"`
}

type uiDiagnosticsProviderCorrectionsResponse struct {
	Items []uiDiagnosticsProviderCorrection `json:"items"`
}

type uiDiagnosticsRepairResponse struct {
	Status string `json:"status"`
}

type uiOperationResult struct {
	Status        string    `json:"status"`
	Event         *uiEvent  `json:"event,omitempty"`
	RelatedEvents []uiEvent `json:"related_events,omitempty"`
	Warnings      []string  `json:"warnings,omitempty"`
}

type uiCalendar struct {
	ID                 string `json:"id"`
	ConnectionID       string `json:"connection_id"`
	Provider           string `json:"provider"`
	ConnectionName     string `json:"connection_name,omitempty"`
	ProviderCalendarID string `json:"provider_calendar_id"`
	Name               string `json:"name"`
	TimeZone           string `json:"time_zone,omitempty"`
	Color              string `json:"color,omitempty"`
	CanRead            bool   `json:"can_read"`
	CanWrite           bool   `json:"can_write"`
	SupportsRecurrence bool   `json:"supports_recurrence"`
}

type uiConnection struct {
	ID             string     `json:"id"`
	Provider       string     `json:"provider"`
	DisplayName    string     `json:"display_name"`
	Status         string     `json:"status"`
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty"`
	LastErrorCode  string     `json:"last_error_code,omitempty"`
}

type uiRule struct {
	ID                  string     `json:"id"`
	SourceCalendarID    string     `json:"source_calendar_id"`
	TargetCalendarID    string     `json:"target_calendar_id"`
	State               string     `json:"state"`
	IntervalSeconds     int        `json:"interval_seconds"`
	LookbackDays        int        `json:"lookback_days"`
	LookaheadDays       int        `json:"lookahead_days"`
	RecurrenceMode      string     `json:"recurrence_mode"`
	NotificationPolicy  string     `json:"notification_policy"`
	NextRunAt           *time.Time `json:"next_run_at,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type uiRun struct {
	ID           string     `json:"id"`
	JobID        string     `json:"job_id"`
	RuleID       string     `json:"rule_id"`
	Trigger      string     `json:"trigger"`
	Outcome      string     `json:"outcome"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	CreatedCount int        `json:"created_count"`
	UpdatedCount int        `json:"updated_count"`
	DeletedCount int        `json:"deleted_count"`
	SkippedCount int        `json:"skipped_count"`
	WarningCount int        `json:"warning_count"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorSummary string     `json:"error_summary,omitempty"`
	DryRun       bool       `json:"dry_run"`
}

type uiSettings struct {
	MCPEndpoint            string `json:"mcp_endpoint"`
	LegacyAPIKeyConfigured bool   `json:"legacy_api_key_configured"`
	GoogleConfigured       bool   `json:"google_configured"`
	MicrosoftConfigured    bool   `json:"microsoft_configured"`
}

type uiControlPlane struct {
	Connections []uiConnection `json:"connections"`
	Calendars   []uiCalendar   `json:"calendars"`
	Rules       []uiRule       `json:"rules"`
	Runs        []uiRun        `json:"runs"`
	Settings    uiSettings     `json:"settings"`
}

type uiBootstrap struct {
	Username               string                                   `json:"username,omitempty"`
	DiagnosticsOperator    bool                                     `json:"diagnostics_operator"`
	CSRFToken              string                                   `json:"csrf_token"`
	Connections            []uiConnection                           `json:"connections"`
	Calendars              []uiCalendar                             `json:"calendars"`
	Rules                  []uiRule                                 `json:"rules"`
	Runs                   []uiRun                                  `json:"runs"`
	Settings               uiSettings                               `json:"settings"`
	MCPEndpoint            string                                   `json:"mcp_endpoint"`
	LegacyAPIKeyConfigured bool                                     `json:"legacy_api_key_configured"`
	Capabilities           map[string]calendar.CalendarCapabilities `json:"capabilities"`
	Sources                []uiSourceStatus                         `json:"sources"`
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	control, err := s.uiControlPlane(r.Context())
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	capabilities := make(map[string]calendar.CalendarCapabilities, len(control.Calendars))
	sources := make([]uiSourceStatus, 0)
	for _, item := range control.Calendars {
		capability, err := s.app.Capabilities(r.Context(), item.ID)
		if err != nil {
			sources = append(sources, failedUISource("", item.ID, err))
			continue
		}
		capabilities[item.ID] = capability
	}
	writeUIJSON(w, http.StatusOK, uiBootstrap{
		Username: r.Header.Get("X-authentik-username"), DiagnosticsOperator: containsString(s.config.RawArtifactOperators, r.Header.Get("X-authentik-username")), CSRFToken: s.csrfToken(w, r),
		Connections: control.Connections, Calendars: control.Calendars, Rules: control.Rules, Runs: control.Runs, Settings: control.Settings,
		MCPEndpoint: control.Settings.MCPEndpoint, LegacyAPIKeyConfigured: control.Settings.LegacyAPIKeyConfigured,
		Capabilities: capabilities, Sources: sources,
	})
}

func (s *Server) controlPlane(w http.ResponseWriter, r *http.Request) {
	control, err := s.uiControlPlane(r.Context())
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, control)
}

func (s *Server) rawSyncArtifact(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	username := strings.TrimSpace(r.Header.Get("X-authentik-username"))
	if username == "" || !containsString(s.config.RawArtifactOperators, username) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if s.store == nil {
		writeUIAPIError(w, errors.New("raw sync artifacts are unavailable"))
		return
	}
	var request rawSyncArtifactRequest
	if err := decodeUIJSON(r, &request); err != nil {
		writeUIAPIError(w, err)
		return
	}
	calendarID := strings.TrimSpace(request.CalendarID)
	objectID := strings.TrimSpace(request.ObjectID)
	diagnostic, err := s.store.GetActiveEventSyncQuarantineDiagnostic(r.Context(), calendarID, objectID)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	artifact, err := s.store.GetRawEventSyncArtifact(r.Context(), calendarID, objectID, diagnostic.ETag)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, uiRawSyncArtifact{CalendarID: artifact.CalendarID, ObjectID: artifact.ObjectID, ETag: artifact.ETag, PayloadBase64: base64.StdEncoding.EncodeToString(artifact.RawPayload), PayloadSHA256: artifact.PayloadSHA256, ContentType: artifact.ContentType, ProviderStatus: artifact.ProviderStatus, ProviderReason: artifact.ProviderReason, Truncated: artifact.Truncated, CapturedAt: artifact.CapturedAt, ExpiresAt: artifact.ExpiresAt})
}

func (s *Server) requireDiagnosticsOperator(w http.ResponseWriter, r *http.Request) bool {
	username := strings.TrimSpace(r.Header.Get("X-authentik-username"))
	if username == "" || !containsString(s.config.RawArtifactOperators, username) {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	if s.store == nil {
		writeUIAPIError(w, errors.New("sync diagnostics are unavailable"))
		return false
	}
	return true
}

func (s *Server) listDiagnosticsQuarantine(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagnosticsOperator(w, r) {
		return
	}
	limit, offset, err := parseDiagnosticsPage(r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	items, err := s.store.ListActiveEventSyncQuarantineDiagnostics(r.Context(), limit, offset)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	result := uiDiagnosticsQuarantineResponse{Items: make([]uiDiagnosticsQuarantine, 0, len(items)), Limit: limit, Offset: offset}
	for _, item := range items {
		result.Items = append(result.Items, uiDiagnosticsQuarantineFromStorage(item))
	}
	writeUIJSON(w, http.StatusOK, result)
}

func (s *Server) listDiagnosticsProviderCorrections(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagnosticsOperator(w, r) {
		return
	}
	items, err := s.store.ListRecentEventSyncProviderCorrections(r.Context(), maxDiagnosticsPageSize)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	result := uiDiagnosticsProviderCorrectionsResponse{Items: make([]uiDiagnosticsProviderCorrection, 0, len(items))}
	for _, item := range items {
		result.Items = append(result.Items, uiDiagnosticsProviderCorrection{
			CalendarID: item.CalendarID, ObjectID: item.ObjectID, Outcome: item.Outcome,
			CorrectedAt: item.CorrectedAt, CalendarName: item.CalendarName, Provider: item.Provider,
		})
	}
	writeUIJSON(w, http.StatusOK, result)
}

func (s *Server) diagnosticsQuarantineDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagnosticsOperator(w, r) {
		return
	}
	var request rawSyncArtifactRequest
	if err := decodeUIJSON(r, &request); err != nil {
		writeUIAPIError(w, err)
		return
	}
	request.CalendarID, request.ObjectID = strings.TrimSpace(request.CalendarID), strings.TrimSpace(request.ObjectID)
	if request.CalendarID == "" || request.ObjectID == "" {
		err := calendar.NewAPIError(calendar.ErrorInvalidArgument, "calendar_id and object_id are required")
		writeUIAPIError(w, err)
		return
	}
	item, err := s.store.GetActiveEventSyncQuarantineDiagnostic(r.Context(), request.CalendarID, request.ObjectID)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, uiDiagnosticsQuarantineFromStorage(*item))
}

func (s *Server) scheduleDiagnosticsRepair(w http.ResponseWriter, r *http.Request) {
	if !s.requireDiagnosticsOperator(w, r) {
		return
	}
	request, err := decodeDiagnosticsQuarantineRequest(r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	scheduled, err := s.store.ScheduleEventSyncObjectRepair(r.Context(), request.CalendarID, request.ObjectID, request.ExpectedETag, time.Now().UTC())
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrEventSyncRepairETagMismatch) {
		writeUIAPIError(w, calendar.NewAPIError(calendar.ErrorConflict, "the quarantined object changed; refresh diagnostics before authorizing repair"))
		return
	}
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	status := "already_queued"
	if scheduled {
		status = "scheduled"
	}
	writeUIJSON(w, http.StatusOK, uiDiagnosticsRepairResponse{Status: status})
}

func parseDiagnosticsPage(r *http.Request) (int, int, error) {
	limit, offset := 50, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxDiagnosticsPageSize {
			return 0, 0, calendar.NewAPIError(calendar.ErrorInvalidArgument, "limit must be between 1 and 100")
		}
		limit = value
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, 0, calendar.NewAPIError(calendar.ErrorInvalidArgument, "offset must not be negative")
		}
		offset = value
	}
	return limit, offset, nil
}

func decodeDiagnosticsQuarantineRequest(r *http.Request) (diagnosticsQuarantineRequest, error) {
	var request diagnosticsQuarantineRequest
	if err := decodeUIJSON(r, &request); err != nil {
		return request, err
	}
	request.CalendarID, request.ObjectID, request.ExpectedETag = strings.TrimSpace(request.CalendarID), strings.TrimSpace(request.ObjectID), strings.TrimSpace(request.ExpectedETag)
	if request.CalendarID == "" || request.ObjectID == "" || request.ExpectedETag == "" {
		return request, calendar.NewAPIError(calendar.ErrorInvalidArgument, "calendar_id, object_id, and expected_etag are required")
	}
	return request, nil
}

func uiDiagnosticsQuarantineFromStorage(item storage.EventSyncQuarantineDiagnostic) uiDiagnosticsQuarantine {
	result := uiDiagnosticsQuarantine{CalendarID: item.CalendarID, ObjectID: item.ObjectID, ETag: item.ETag, ErrorCode: item.ErrorCode, CalendarName: item.CalendarName, Provider: item.Provider, SyncStatus: item.SyncStatus, LastErrorCode: item.LastErrorCode, FirstSeenAt: item.FirstSeenAt, LastSeenAt: item.LastSeenAt, NextRepairAt: item.NextRepairAt, RepairAttempts: item.RepairAttempts, LastSuccessAt: item.LastSuccessAt}
	if item.Artifact != nil {
		result.Artifact = &uiRawSyncArtifactMetadata{ETag: item.Artifact.ETag, PayloadSHA256: item.Artifact.PayloadSHA256, ContentType: item.Artifact.ContentType, ProviderStatus: item.Artifact.ProviderStatus, ProviderReason: item.Artifact.ProviderReason, Truncated: item.Artifact.Truncated, CapturedAt: item.Artifact.CapturedAt, ExpiresAt: item.Artifact.ExpiresAt}
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func (s *Server) uiControlPlane(ctx context.Context) (uiControlPlane, error) {
	connectionsList, err := s.store.ListConnections(ctx)
	if err != nil {
		return uiControlPlane{}, fmt.Errorf("list connections: %w", err)
	}
	calendars, err := s.store.ListAllCalendars(ctx)
	if err != nil {
		return uiControlPlane{}, fmt.Errorf("list calendars: %w", err)
	}
	rules, err := s.store.ListRules(ctx)
	if err != nil {
		return uiControlPlane{}, fmt.Errorf("list rules: %w", err)
	}
	runs, err := s.store.ListRuns(ctx, 50)
	if err != nil {
		return uiControlPlane{}, fmt.Errorf("list runs: %w", err)
	}
	result := uiControlPlane{
		Connections: make([]uiConnection, 0, len(connectionsList)), Calendars: make([]uiCalendar, 0, len(calendars)),
		Rules: make([]uiRule, 0, len(rules)), Runs: make([]uiRun, 0, len(runs)),
		Settings: uiSettings{MCPEndpoint: strings.TrimSuffix(s.config.PublicURL, "/") + "/mcp", LegacyAPIKeyConfigured: s.config.LegacyAPIKeyConfigured, GoogleConfigured: s.config.GoogleConfigured, MicrosoftConfigured: s.config.MicrosoftConfigured},
	}
	connectionProviders := make(map[string]string, len(connectionsList))
	connectionNames := make(map[string]string, len(connectionsList))
	connected := make(map[string]bool, len(connectionsList))
	for _, item := range connectionsList {
		result.Connections = append(result.Connections, uiConnection{ID: item.ID, Provider: item.Provider, DisplayName: item.DisplayName, Status: item.Status, LastVerifiedAt: item.LastVerifiedAt, LastErrorCode: item.LastErrorCode})
		connectionProviders[item.ID] = item.Provider
		connectionNames[item.ID] = item.DisplayName
		connected[item.ID] = item.Status == "connected"
	}
	for _, item := range calendars {
		if !connected[item.ConnectionID] {
			continue
		}
		result.Calendars = append(result.Calendars, uiCalendar{ID: item.ID, ConnectionID: item.ConnectionID, Provider: connectionProviders[item.ConnectionID], ConnectionName: connectionNames[item.ConnectionID], ProviderCalendarID: item.ProviderCalendarID, Name: item.Name, TimeZone: item.Timezone, CanRead: item.CanRead, CanWrite: item.CanWrite, SupportsRecurrence: item.SupportsRecurrence})
	}
	for _, item := range rules {
		result.Rules = append(result.Rules, uiRule{ID: item.ID, SourceCalendarID: item.SourceCalendarID, TargetCalendarID: item.TargetCalendarID, State: item.State, IntervalSeconds: item.IntervalSeconds, LookbackDays: item.LookbackDays, LookaheadDays: item.LookaheadDays, RecurrenceMode: item.RecurrenceMode, NotificationPolicy: item.NotificationPolicy, NextRunAt: item.NextRunAt, ConsecutiveFailures: item.ConsecutiveFailures, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
	}
	for _, item := range runs {
		result.Runs = append(result.Runs, uiRun{ID: item.ID, JobID: item.JobID, RuleID: item.RuleID, Trigger: item.Trigger, Outcome: item.Outcome, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, CreatedCount: item.CreatedCount, UpdatedCount: item.UpdatedCount, DeletedCount: item.DeletedCount, SkippedCount: item.SkippedCount, WarningCount: item.WarningCount, ErrorCode: item.ErrorCode, ErrorSummary: safeRunSummary(item), DryRun: item.DryRun})
	}
	return result, nil
}

func safeRunSummary(run storage.Run) string {
	if run.ErrorSummary == "" {
		return ""
	}
	return "Run failed; inspect server logs for details."
}

func (s *Server) listUIEvents(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	start, end, err := parseUIEventRange(r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	calendarIDs := uniqueStrings(r.URL.Query()["calendar_id"])
	if s.eventReadModelEnabled {
		response, err := s.listCachedUIEvents(r.Context(), calendarIDs, start, end)
		if err != nil {
			writeUIAPIError(w, err)
			return
		}
		response.sort()
		writeUIJSON(w, http.StatusOK, response)
		return
	}
	response := uiEventsResponse{Items: []uiEvent{}, Complete: true, Sources: []uiSourceStatus{}}
	if len(calendarIDs) == 0 {
		page, err := s.listUIDrainedEvents(r, "", start, end)
		response.appendPage(page)
		if err != nil {
			response.Complete = false
			response.Sources = append(response.Sources, failedUISource("", "", err))
		}
	} else {
		for _, calendarID := range calendarIDs {
			page, err := s.listUIDrainedEvents(r, calendarID, start, end)
			response.appendPage(page)
			if err != nil {
				response.Complete = false
				response.Sources = append(response.Sources, failedUISource("", calendarID, err))
				continue
			}
		}
	}
	response.sort()
	writeUIJSON(w, http.StatusOK, response)
}

func (s *Server) listCachedUIEvents(ctx context.Context, calendarIDs []string, start, end time.Time) (uiEventsResponse, error) {
	if len(calendarIDs) == 0 {
		control, err := s.uiControlPlane(ctx)
		if err != nil {
			return uiEventsResponse{}, err
		}
		for _, item := range control.Calendars {
			if item.CanRead {
				calendarIDs = append(calendarIDs, item.ID)
			}
		}
	}
	calendarIDs = uniqueStrings(calendarIDs)
	events, sources, err := s.app.ListCachedEvents(ctx, calendarIDs, start, end)
	if err != nil {
		return uiEventsResponse{}, err
	}
	response := uiEventsResponse{Items: make([]uiEvent, 0, len(events)), Sources: make([]uiSourceStatus, 0, len(calendarIDs)), Complete: true}
	for _, event := range events {
		response.Items = append(response.Items, toUIEvent(normalizeCachedUIEvent(event)))
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		response.Sources = append(response.Sources, toCachedUISource(source))
		seen[source.CalendarID] = struct{}{}
	}
	for _, calendarID := range calendarIDs {
		if _, ok := seen[calendarID]; ok {
			continue
		}
		// A readable calendar with no durable state has not been warmed yet.
		response.Sources = append(response.Sources, uiSourceStatus{Provider: uiCalendarProvider(calendarID), CalendarID: calendarID, Status: "pending", Stale: true})
	}
	for _, source := range response.Sources {
		if source.Status != "ready" || source.Stale {
			response.Complete = false
		}
	}
	return response, nil
}

func (s *Server) refreshUICalendar(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	calendarID := r.PathValue("id")
	if calendarID == "" {
		writeUIAPIError(w, calendar.NewAPIError(calendar.ErrorInvalidArgument, "calendar_id is required"))
		return
	}
	if err := s.app.ScheduleCalendarSync(r.Context(), calendarID, time.Now().UTC()); err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrCalendarSyncIneligible) {
			writeUIAPIError(w, calendar.NewAPIError(calendar.ErrorNotFound, "calendar refresh is unavailable"))
			return
		}
		if errors.Is(err, storage.ErrCalendarSyncActive) {
			writeUIAPIError(w, calendar.NewAPIError(calendar.ErrorConflict, "calendar refresh is already in progress"))
			return
		}
		writeUIAPIError(w, err)
		return
	}
	writeUIJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) listUIDrainedEvents(r *http.Request, calendarID string, start, end time.Time) (calendar.Page[calendar.EventV2], error) {
	ctx, cancel := context.WithTimeout(r.Context(), uiEventSourceTimeout)
	defer cancel()
	return drainUIEventPages(ctx, calendar.ListEventsRequestV2{CalendarID: calendarID, Start: start, End: end, View: calendar.RecurrenceExpanded}, s.app.ListEvents)
}

func drainUIEventPages(ctx context.Context, request calendar.ListEventsRequestV2, fetch func(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error)) (calendar.Page[calendar.EventV2], error) {
	result := calendar.Page[calendar.EventV2]{Complete: true}
	seen := map[string]struct{}{}
	for pageNumber := 0; pageNumber < maxUIEventPages; pageNumber++ {
		page, err := fetch(ctx, request)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, page.Items...)
		result.Sources = append(result.Sources, page.Sources...)
		result.Complete = result.Complete && page.Complete
		if page.NextPageToken == "" {
			return result, nil
		}
		if _, duplicate := seen[page.NextPageToken]; duplicate {
			return result, calendar.NewAPIError(calendar.ErrorProviderUnavailable, "provider pagination repeated a page token")
		}
		seen[page.NextPageToken] = struct{}{}
		request.PageToken = page.NextPageToken
	}
	return result, calendar.NewAPIError(calendar.ErrorProviderUnavailable, "provider pagination exceeded the browser safety limit")
}

func (s *Server) getUIEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	ref, err := uiEventRef(r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	event, err := s.app.GetEvent(r.Context(), ref)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, toUIEvent(*event))
}

func (s *Server) createUIEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	var input uiCreateEventInput
	if err := decodeUIInput(w, r, &input); err != nil {
		writeUIAPIError(w, err)
		return
	}
	event, warnings, err := s.app.CreateEventWithReconciliation(r.Context(), calendar.CreateEventRequestV2{
		CalendarID:    input.CalendarID,
		Event:         calendar.EventCreateV2{Title: input.Title, Description: input.Description, Location: input.Location, Start: input.Start, End: input.End, Recurrence: input.Recurrence, Visibility: input.Visibility, Transparency: input.Transparency, ColorID: input.ColorID},
		Notifications: calendar.NotificationsNone,
	})
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	response := toUIEvent(*event)
	response.Warnings = warnings
	writeUIJSON(w, http.StatusCreated, response)
}

func (s *Server) updateUIEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	ref, err := uiEventRef(r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	input, err := decodeUIPatchInput(w, r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	result, err := s.app.UpdateEvent(r.Context(), calendar.UpdateEventRequestV2{Ref: ref, Patch: input.patch, Scope: input.scope, EffectiveFrom: input.effectiveFrom, ExpectedETag: input.expectedETag, OperationID: input.operationID, Notifications: calendar.NotificationsNone})
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, toUIOperation(*result))
}

func (s *Server) deleteUIEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.requireApplication(); err != nil {
		writeUIAPIError(w, err)
		return
	}
	ref, err := uiEventRef(r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	input, err := decodeUIDeleteInput(w, r)
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	result, err := s.app.DeleteEvent(r.Context(), calendar.DeleteEventRequestV2{Ref: ref, Scope: input.scope, EffectiveFrom: input.effectiveFrom, ExpectedETag: input.expectedETag, OperationID: input.operationID, Notifications: calendar.NotificationsNone})
	if err != nil {
		writeUIAPIError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, toUIOperation(*result))
}

type uiCreateEventInput struct {
	CalendarID   string             `json:"calendar_id"`
	Title        string             `json:"title"`
	Description  string             `json:"description"`
	Location     string             `json:"location"`
	Start        calendar.EventTime `json:"start"`
	End          calendar.EventTime `json:"end"`
	Recurrence   []string           `json:"recurrence"`
	Visibility   string             `json:"visibility"`
	Transparency string             `json:"transparency"`
	ColorID      string             `json:"color_id"`
}

type uiPatchInput struct {
	patch         calendar.EventPatchV2
	scope         calendar.MutationScope
	effectiveFrom *calendar.EventTime
	expectedETag  string
	operationID   string
}

type uiDeleteInput struct {
	scope         calendar.MutationScope
	effectiveFrom *calendar.EventTime
	expectedETag  string
	operationID   string
}

func decodeUIPatchInput(w http.ResponseWriter, r *http.Request) (uiPatchInput, error) {
	values, err := decodeUIRawObject(w, r)
	if err != nil {
		return uiPatchInput{}, err
	}
	if err := validateUIFields(values, "scope", "effective_from", "expected_etag", "operation_id", "title", "description", "location", "start", "end", "visibility", "transparency", "color_id"); err != nil {
		return uiPatchInput{}, err
	}
	scope, err := readOptionalString(values, "scope")
	if err != nil {
		return uiPatchInput{}, err
	}
	expectedETag, err := readOptionalString(values, "expected_etag")
	if err != nil {
		return uiPatchInput{}, err
	}
	operationID, err := readOptionalString(values, "operation_id")
	if err != nil {
		return uiPatchInput{}, err
	}
	result := uiPatchInput{scope: calendar.MutationScope(scope), expectedETag: expectedETag, operationID: operationID}
	if raw, ok := values["effective_from"]; ok {
		var value calendar.EventTime
		if err := json.Unmarshal(raw, &value); err != nil {
			return uiPatchInput{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, "effective_from must be an event time")
		}
		result.effectiveFrom = &value
	}
	if result.patch.Title, err = stringPatch(values, "title"); err != nil {
		return uiPatchInput{}, err
	}
	if result.patch.Description, err = stringPatch(values, "description"); err != nil {
		return uiPatchInput{}, err
	}
	if result.patch.Location, err = stringPatch(values, "location"); err != nil {
		return uiPatchInput{}, err
	}
	if result.patch.Visibility, err = stringPatch(values, "visibility"); err != nil {
		return uiPatchInput{}, err
	}
	if result.patch.Transparency, err = stringPatch(values, "transparency"); err != nil {
		return uiPatchInput{}, err
	}
	if result.patch.ColorID, err = stringPatch(values, "color_id"); err != nil {
		return uiPatchInput{}, err
	}
	if result.patch.Start, err = eventTimePatch(values, "start"); err != nil {
		return uiPatchInput{}, err
	}
	if result.patch.End, err = eventTimePatch(values, "end"); err != nil {
		return uiPatchInput{}, err
	}
	return result, nil
}

func decodeUIDeleteInput(w http.ResponseWriter, r *http.Request) (uiDeleteInput, error) {
	values, err := decodeUIRawObject(w, r)
	if err != nil {
		return uiDeleteInput{}, err
	}
	if err := validateUIFields(values, "scope", "effective_from", "expected_etag", "operation_id"); err != nil {
		return uiDeleteInput{}, err
	}
	scope, err := readOptionalString(values, "scope")
	if err != nil {
		return uiDeleteInput{}, err
	}
	expectedETag, err := readOptionalString(values, "expected_etag")
	if err != nil {
		return uiDeleteInput{}, err
	}
	operationID, err := readOptionalString(values, "operation_id")
	if err != nil {
		return uiDeleteInput{}, err
	}
	result := uiDeleteInput{scope: calendar.MutationScope(scope), expectedETag: expectedETag, operationID: operationID}
	if raw, ok := values["effective_from"]; ok {
		var value calendar.EventTime
		if err := json.Unmarshal(raw, &value); err != nil {
			return uiDeleteInput{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, "effective_from must be an event time")
		}
		result.effectiveFrom = &value
	}
	return result, nil
}

func decodeUIInput(w http.ResponseWriter, r *http.Request, target *uiCreateEventInput) error {
	values, err := decodeUIRawObject(w, r)
	if err != nil {
		return err
	}
	if err := validateUIFields(values, "calendar_id", "title", "description", "location", "start", "end", "recurrence", "visibility", "transparency", "color_id"); err != nil {
		return err
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid event input")
	}
	return nil
}

func decodeUIJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxUIRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return calendar.NewAPIError(calendar.ErrorInvalidArgument, "invalid JSON request")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return calendar.NewAPIError(calendar.ErrorInvalidArgument, "request body must contain one JSON value")
	}
	return nil
}

func decodeUIRawObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUIRequestBodyBytes))
	decoder.DisallowUnknownFields()
	values := map[string]json.RawMessage{}
	if err := decoder.Decode(&values); err != nil {
		return nil, calendar.NewAPIError(calendar.ErrorInvalidArgument, "request body must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, calendar.NewAPIError(calendar.ErrorInvalidArgument, "request body must contain one JSON value")
	}
	return values, nil
}

func validateUIFields(values map[string]json.RawMessage, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = struct{}{}
	}
	for field := range values {
		if isUnsafeUIField(field) {
			return calendar.NewAPIError(calendar.ErrorInvalidArgument, "field "+field+" is not supported by the browser calendar")
		}
		if _, ok := allowedSet[field]; !ok {
			return calendar.NewAPIError(calendar.ErrorInvalidArgument, "unknown browser calendar field "+field)
		}
	}
	return nil
}

func isUnsafeUIField(field string) bool {
	switch field {
	case "attendees", "notifications", "notification_policy", "conference", "conference_data", "reminders", "attachments", "guest_permissions", "video_call", "guests_can_invite_others", "guests_can_modify", "guests_can_see_other_guests":
		return true
	default:
		return false
	}
}

func readOptionalString(values map[string]json.RawMessage, key string) (string, error) {
	raw, ok := values[key]
	if !ok {
		return "", nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", calendar.NewAPIError(calendar.ErrorInvalidArgument, key+" must be a string or null")
	}
	return result, nil
}

func stringPatch(values map[string]json.RawMessage, key string) (calendar.PatchField[string], error) {
	raw, ok := values[key]
	if !ok {
		return calendar.PatchField[string]{}, nil
	}
	result := calendar.PatchField[string]{Present: true}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		result.Null = true
		return result, nil
	}
	if err := json.Unmarshal(raw, &result.Value); err != nil {
		return calendar.PatchField[string]{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, key+" must be a string or null")
	}
	return result, nil
}

func eventTimePatch(values map[string]json.RawMessage, key string) (calendar.PatchField[calendar.EventTime], error) {
	raw, ok := values[key]
	if !ok {
		return calendar.PatchField[calendar.EventTime]{}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return calendar.PatchField[calendar.EventTime]{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, key+" cannot be null")
	}
	result := calendar.PatchField[calendar.EventTime]{Present: true}
	if err := json.Unmarshal(raw, &result.Value); err != nil {
		return calendar.PatchField[calendar.EventTime]{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, key+" must be an event time")
	}
	return result, nil
}

func parseUIEventRange(r *http.Request) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339, r.URL.Query().Get("start"))
	if err != nil {
		return time.Time{}, time.Time{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, "start must be RFC3339")
	}
	end, err := time.Parse(time.RFC3339, r.URL.Query().Get("end"))
	if err != nil {
		return time.Time{}, time.Time{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, "end must be RFC3339")
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, "end must be after start")
	}
	if end.Sub(start) > maxUIEventRange {
		return time.Time{}, time.Time{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, "requested calendar range is too large")
	}
	return start, end, nil
}

func uiEventRef(r *http.Request) (calendar.EventRef, error) {
	ref := calendar.EventRef{CalendarID: r.URL.Query().Get("calendar_id"), EventID: r.URL.Query().Get("event_id")}
	if ref.CalendarID == "" || ref.EventID == "" {
		return calendar.EventRef{}, calendar.NewAPIError(calendar.ErrorInvalidArgument, "calendar and event IDs are required")
	}
	return ref, nil
}

func (s *Server) requireApplication() error {
	if s.app == nil {
		return calendar.NewAPIError(calendar.ErrorProviderUnavailable, "calendar browser API is unavailable")
	}
	return nil
}

func (response *uiEventsResponse) appendPage(page calendar.Page[calendar.EventV2]) {
	for _, event := range page.Items {
		response.Items = append(response.Items, toUIEvent(event))
	}
	for _, source := range page.Sources {
		response.Sources = append(response.Sources, toUISource(source))
	}
	response.Complete = response.Complete && page.Complete
}

func (response *uiEventsResponse) sort() {
	sort.SliceStable(response.Items, func(i, j int) bool {
		left, right := response.Items[i], response.Items[j]
		leftStart, leftOK := left.Start.Instant()
		rightStart, rightOK := right.Start.Instant()
		if leftOK == nil && rightOK == nil && !leftStart.Equal(rightStart) {
			return leftStart.Before(rightStart)
		}
		if leftOK == nil && rightOK == nil && left.Start.IsAllDay() != right.Start.IsAllDay() {
			return left.Start.IsAllDay()
		}
		if leftOK == nil && rightOK != nil {
			return true
		}
		if leftOK != nil && rightOK == nil {
			return false
		}
		leftValue, rightValue := left.Start.Date+left.Start.DateTime, right.Start.Date+right.Start.DateTime
		if leftValue != rightValue {
			return leftValue < rightValue
		}
		if left.CalendarID != right.CalendarID {
			return left.CalendarID < right.CalendarID
		}
		return left.ID < right.ID
	})
	sort.SliceStable(response.Sources, func(i, j int) bool {
		if response.Sources[i].Provider != response.Sources[j].Provider {
			return response.Sources[i].Provider < response.Sources[j].Provider
		}
		return response.Sources[i].CalendarID < response.Sources[j].CalendarID
	})
}

func toUIEvent(event calendar.EventV2) uiEvent {
	return uiEvent{ID: event.ID, CalendarID: event.CalendarID, Provider: event.Provider, ETag: event.ETag, Title: event.Title, Description: event.Description, Location: event.Location, Status: event.Status, Start: event.Start, End: event.End, OriginalStart: event.OriginalStart, RecurringEventID: event.RecurringEventID, InstanceKind: event.InstanceKind, Recurrence: event.Recurrence, Visibility: event.Visibility, Transparency: event.Transparency, ColorID: event.ColorID, Created: event.Created, Updated: event.Updated, ReadOnly: event.ReadOnly}
}

func toUIOperation(result calendar.OperationResult) uiOperationResult {
	converted := uiOperationResult{Status: result.Status, RelatedEvents: make([]uiEvent, 0, len(result.RelatedEvents)), Warnings: append([]string(nil), result.Warnings...)}
	if result.Event != nil {
		event := toUIEvent(*result.Event)
		converted.Event = &event
	}
	for _, event := range result.RelatedEvents {
		converted.RelatedEvents = append(converted.RelatedEvents, toUIEvent(event))
	}
	return converted
}

func toUISource(source calendar.SourceStatus) uiSourceStatus {
	status := "ready"
	if !source.Complete {
		status = "failed"
	}
	result := uiSourceStatus{Provider: source.Provider, CalendarID: source.CalendarID, Complete: source.Complete, Status: status}
	if source.Error != nil {
		converted := safeUIError(source.Error)
		result.Error = &converted
	}
	return result
}

func failedUISource(provider, calendarID string, err error) uiSourceStatus {
	if provider == "" {
		provider = uiCalendarProvider(calendarID)
	}
	converted := safeUIError(err)
	return uiSourceStatus{Provider: provider, CalendarID: calendarID, Complete: false, Error: &converted, Status: "failed"}
}

func toCachedUISource(source storage.CachedSourceStatus) uiSourceStatus {
	status := source.Status
	if !isUISyncStatus(status) {
		status = "failed"
	}
	result := uiSourceStatus{Provider: source.Provider, CalendarID: source.CalendarID, Status: status, LastSuccessAt: source.LastSuccessAt, Stale: source.Stale}
	result.Complete = status == "ready" && !source.Stale
	if code, ok := safeUISyncErrorCode(source.ErrorCode); ok {
		result.ErrorCode = &code
	}
	return result
}

func normalizeCachedUIEvent(event calendar.EventV2) calendar.EventV2 {
	provider := event.Provider
	if provider == "" {
		provider = uiCalendarProvider(event.CalendarID)
		event.Provider = provider
	}
	if provider != "" && event.ID != "" && !strings.HasPrefix(event.ID, provider+":") {
		event.ID = provider + ":" + event.ID
	}
	if provider != "" && event.RecurringEventID != "" && !strings.HasPrefix(event.RecurringEventID, provider+":") {
		event.RecurringEventID = provider + ":" + event.RecurringEventID
	}
	return event
}

func isUISyncStatus(value string) bool {
	switch value {
	case "pending", "syncing", "ready", "failed", "parked", "degraded":
		return true
	default:
		return false
	}
}

func safeUISyncErrorCode(value string) (string, bool) {
	switch value {
	case "transient", "rate_limited", "auth", "permission", "unsupported", "protocol":
		return value, true
	default:
		return "", false
	}
}

func uiCalendarProvider(calendarID string) string {
	provider, _, found := strings.Cut(calendarID, ":")
	if !found {
		return ""
	}
	return provider
}

func safeUIError(err error) uiError {
	apiErr := &calendar.APIError{}
	if !errors.As(err, &apiErr) {
		return uiError{Code: calendar.ErrorProviderUnavailable, Message: "Calendar provider is temporarily unavailable", Retryable: true, Origin: uiErrorOriginApplication}
	}
	message := "Calendar operation could not be completed"
	switch apiErr.Code {
	case calendar.ErrorInvalidArgument:
		message = "The calendar request is invalid"
	case calendar.ErrorInvalidRecurrence:
		message = "The event recurrence is invalid"
	case calendar.ErrorUnsupportedCapability:
		message = "This calendar operation is not supported"
	case calendar.ErrorConflict:
		message = "The event changed elsewhere; refresh and try again"
	case calendar.ErrorProviderUnavailable:
		message = "Calendar provider is temporarily unavailable"
	case calendar.ErrorPermissionDenied:
		message = "The calendar provider denied this operation"
	case calendar.ErrorNotFound:
		message = "The requested calendar event was not found"
	case calendar.ErrorRateLimited:
		message = "The calendar provider rate limited this operation"
	case calendar.ErrorPartialFailure:
		message = "Some calendar sources could not be loaded"
	}
	return uiError{Code: apiErr.Code, Message: message, Retryable: apiErr.Retryable, Origin: uiErrorOriginApplication}
}

func writeUIAPIError(w http.ResponseWriter, err error) {
	apiErr := safeUIError(err)
	status := http.StatusBadGateway
	switch apiErr.Code {
	case calendar.ErrorInvalidArgument, calendar.ErrorInvalidRecurrence:
		status = http.StatusBadRequest
	case calendar.ErrorUnsupportedCapability:
		status = http.StatusUnprocessableEntity
	case calendar.ErrorNotFound:
		status = http.StatusNotFound
	case calendar.ErrorPermissionDenied:
		status = http.StatusForbidden
	case calendar.ErrorConflict:
		status = http.StatusConflict
	case calendar.ErrorRateLimited:
		status = http.StatusTooManyRequests
	}
	writeUISafeError(w, status, apiErr)
}

func writeUIError(w http.ResponseWriter, status int, err error) {
	writeUISafeError(w, status, safeUIError(err))
}

func writeUISafeError(w http.ResponseWriter, status int, apiErr uiError) {
	// Log only the normalized, public contract. Raw provider errors may contain
	// credentials, calendar paths, or event identifiers and must not reach logs.
	log.Printf("calendar_ui_error error_origin=%s http_status=%d error_code=%s retryable=%t", apiErr.Origin, status, apiErr.Code, apiErr.Retryable)
	writeUIJSON(w, status, map[string]uiError{"error": apiErr})
}

func writeUIJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("calendar UI JSON response: %v", err)
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
