package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
)

func TestModernAndCompatV2ReadBehaviorMatchesAndLegacyListStaysDistinct(t *testing.T) {
	provider := &catalogBehaviorProvider{
		name:         "google",
		legacyEvents: []calendar.Event{{ID: "legacy-event", CalendarID: "primary", Provider: "google", Title: "Legacy result"}},
		page: calendar.Page[calendar.EventV2]{
			Items:         []calendar.EventV2{{ID: "v2-event", CalendarID: "primary", Title: "V2 result"}},
			NextPageToken: "next-page-token",
			Complete:      false,
			Sources: []calendar.SourceStatus{{
				Provider: "google", CalendarID: "primary", Complete: false,
				Error: &calendar.APIError{Code: calendar.ErrorPartialFailure, Message: "one source is incomplete"},
			}},
		},
		capabilities: calendar.CalendarCapabilities{
			MutationScopes:       []calendar.MutationScope{calendar.ScopeSingle},
			NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
		},
	}
	mux, endpoint := behaviorMux(t, provider, CatalogModern)
	_, modernSession := listHTTPTools(t, mux, endpoint+"/mcp/modern", "key")
	_, compatSession := listHTTPTools(t, mux, endpoint+"/mcp/compat", "key")

	args := map[string]any{
		"calendar_id": "google:primary", "start": "2026-10-01T00:00:00Z", "end": "2026-10-03T00:00:00Z",
		"view": "series", "show_deleted": true, "page_token": "continuation-from-client", "max_results": float64(7),
	}
	modern := callHTTPTool(t, mux, endpoint+"/mcp/modern", "key", modernSession, "get_events", args)
	compatV2 := callHTTPTool(t, mux, endpoint+"/mcp/compat", "key", compatSession, "get_events_v2", args)
	modernJSON, _ := json.Marshal(modern)
	compatJSON, _ := json.Marshal(compatV2)
	if string(modernJSON) != string(compatJSON) {
		t.Fatalf("renamed V2 read result differs: modern=%s compat=%s", modernJSON, compatJSON)
	}
	if modern["isError"] == true {
		t.Fatalf("V2 event list returned error: %#v", modern)
	}
	if len(provider.listRequests) != 2 {
		t.Fatalf("provider received %d V2 list requests, want 2", len(provider.listRequests))
	}
	for _, request := range provider.listRequests {
		if request.CalendarID != "primary" || request.PageToken != "continuation-from-client" || request.MaxResults != 7 || request.View != calendar.RecurrenceSeries || !request.ShowDeleted {
			t.Errorf("provider received unexpected list request: %#v", request)
		}
	}
	structured, ok := modern["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("V2 result has no structured content: %#v", modern)
	}
	if structured["complete"] != false || structured["next_page_token"] != "next-page-token" {
		t.Fatalf("partial page metadata missing: %#v", structured)
	}
	if sources, ok := structured["sources"].([]any); !ok || len(sources) != 1 {
		t.Fatalf("source status missing: %#v", structured["sources"])
	}
	content, ok := modern["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("V2 result text content missing: %#v", modern["content"])
	}

	legacy := callHTTPTool(t, mux, endpoint+"/mcp/compat", "key", compatSession, "get_events", map[string]any{
		"calendar_id": "google:primary", "start": "2026-10-01T00:00:00Z", "end": "2026-10-03T00:00:00Z",
	})
	if legacy["isError"] == true || legacy["structuredContent"] != nil {
		t.Fatalf("legacy list behavior changed: %#v", legacy)
	}
	legacyText := resultText(t, legacy)
	var legacyEvents []calendar.Event
	if err := json.Unmarshal([]byte(legacyText), &legacyEvents); err != nil {
		t.Fatalf("decode legacy event list text %q: %v", legacyText, err)
	}
	if len(legacyEvents) != 1 || legacyEvents[0].Title != "Legacy result" {
		t.Fatalf("legacy event list = %#v", legacyEvents)
	}
}

func TestModernAndCompatV2WritesPreservePolicyPatchAndPreview(t *testing.T) {
	provider := &catalogBehaviorProvider{
		name: "google",
		capabilities: calendar.CalendarCapabilities{
			MutationScopes:       []calendar.MutationScope{calendar.ScopeSingle},
			NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
		},
		created: &calendar.EventV2{ID: "created", CalendarID: "primary", Title: "created"},
		updated: &calendar.OperationResult{Status: "preview", Event: &calendar.EventV2{ID: "event-1", CalendarID: "primary"}},
	}
	mux, endpoint := behaviorMux(t, provider, CatalogModern)
	_, modernSession := listHTTPTools(t, mux, endpoint+"/mcp/modern", "key")
	_, compatSession := listHTTPTools(t, mux, endpoint+"/mcp/compat", "key")

	createArgs := map[string]any{
		"calendar_id": "google:primary",
		"event": map[string]any{
			"title": "Created through catalog", "start": map[string]any{"date": "2026-10-26"}, "end": map[string]any{"date": "2026-10-27"},
		},
	}
	createdModern := callHTTPTool(t, mux, endpoint+"/mcp/modern", "key", modernSession, "create_event", createArgs)
	createExplicit := map[string]any{"calendar_id": "google:primary", "event": createArgs["event"], "notification_policy": "none"}
	createdCompat := callHTTPTool(t, mux, endpoint+"/mcp/compat", "key", compatSession, "create_event_v2", createExplicit)
	if createdModern["isError"] == true || createdCompat["isError"] == true {
		t.Fatalf("supported create returned error: modern=%#v compat=%#v", createdModern, createdCompat)
	}
	if !reflect.DeepEqual(createdModern, createdCompat) {
		t.Fatalf("create result differs across catalogs: modern=%#v compat=%#v", createdModern, createdCompat)
	}
	if len(provider.createRequests) != 2 || provider.createRequests[0].Notifications != calendar.NotificationsNone || provider.createRequests[1].Notifications != calendar.NotificationsNone {
		t.Fatalf("create notification defaults/explicit none differ: %#v", provider.createRequests)
	}

	patchArgs := map[string]any{
		"calendar_id": "google:primary", "event_id": "google:event-1",
		"patch": map[string]any{"title": "", "location": "Room 4"},
		"scope": "single", "expected_etag": "etag-42", "operation_id": "op-preview-7",
		"preview_only": true, "notification_policy": "none",
	}
	updated := callHTTPTool(t, mux, endpoint+"/mcp/modern", "key", modernSession, "update_event", patchArgs)
	if updated["isError"] == true {
		t.Fatalf("preview update returned error: %#v", updated)
	}
	updatedCompat := callHTTPTool(t, mux, endpoint+"/mcp/compat", "key", compatSession, "update_event_v2", patchArgs)
	if !reflect.DeepEqual(updated, updatedCompat) {
		t.Fatalf("update result differs across catalogs: modern=%#v compat=%#v", updated, updatedCompat)
	}
	if len(provider.updateRequests) != 2 || !reflect.DeepEqual(provider.updateRequests[0], provider.updateRequests[1]) {
		t.Fatalf("update provider requests differ across catalogs: %#v", provider.updateRequests)
	}
	request := provider.updateRequests[0]
	if request.Ref.CalendarID != "primary" || request.Ref.EventID != "event-1" || request.Scope != calendar.ScopeSingle || request.ExpectedETag != "etag-42" || request.OperationID != "op-preview-7" || !request.PreviewOnly || request.Notifications != calendar.NotificationsNone {
		t.Fatalf("update request fields were not forwarded: %#v", request)
	}
	if !request.Patch.Title.Present || request.Patch.Title.Null || request.Patch.Title.Value != "" {
		t.Fatalf("explicit empty title was not preserved: %#v", request.Patch.Title)
	}
	if !request.Patch.Location.Present || request.Patch.Location.Value != "Room 4" {
		t.Fatalf("location patch was not preserved: %#v", request.Patch.Location)
	}
	if got := resultText(t, updated); !strings.Contains(got, "preview") {
		t.Fatalf("preview response text = %q", got)
	}

	deleteArgs := map[string]any{
		"calendar_id": "google:primary", "event_id": "google:event-1", "scope": "single",
		"expected_etag": "etag-delete", "operation_id": "op-delete", "preview_only": true, "notification_policy": "none",
	}
	deletedModern := callHTTPTool(t, mux, endpoint+"/mcp/modern", "key", modernSession, "delete_event", deleteArgs)
	deletedCompat := callHTTPTool(t, mux, endpoint+"/mcp/compat", "key", compatSession, "delete_event_v2", deleteArgs)
	if deletedModern["isError"] == true || !reflect.DeepEqual(deletedModern, deletedCompat) {
		t.Fatalf("delete result differs across catalogs: modern=%#v compat=%#v", deletedModern, deletedCompat)
	}
	if len(provider.deleteRequests) != 2 || !reflect.DeepEqual(provider.deleteRequests[0], provider.deleteRequests[1]) {
		t.Fatalf("delete provider requests differ across catalogs: %#v", provider.deleteRequests)
	}
	deleteRequest := provider.deleteRequests[0]
	if deleteRequest.Ref.EventID != "event-1" || deleteRequest.ExpectedETag != "etag-delete" || deleteRequest.OperationID != "op-delete" || !deleteRequest.PreviewOnly || deleteRequest.Notifications != calendar.NotificationsNone {
		t.Fatalf("delete request fields were not forwarded: %#v", deleteRequest)
	}
	beforeCreates, beforeUpdates := len(provider.createRequests), len(provider.updateRequests)
	unsupportedCreate := callHTTPTool(t, mux, endpoint+"/mcp/modern", "key", modernSession, "create_event", map[string]any{
		"calendar_id": "google:primary", "event": createArgs["event"], "notification_policy": "all",
	})
	unsupportedScope := callHTTPTool(t, mux, endpoint+"/mcp/modern", "key", modernSession, "update_event", map[string]any{
		"calendar_id": "google:primary", "event_id": "event-1", "patch": map[string]any{"title": "blocked"},
		"scope": "series", "notification_policy": "none",
	})
	if unsupportedCreate["isError"] != true || unsupportedScope["isError"] != true {
		t.Fatalf("unsupported write policy/scope was accepted: create=%#v update=%#v", unsupportedCreate, unsupportedScope)
	}
	if len(provider.createRequests) != beforeCreates || len(provider.updateRequests) != beforeUpdates {
		t.Fatalf("provider received unsupported writes: creates=%d updates=%d", len(provider.createRequests), len(provider.updateRequests))
	}
}

func TestModernCatalogRejectsCompatV2ToolNameAtCallTime(t *testing.T) {
	provider := &catalogBehaviorProvider{name: "google"}
	mux, endpoint := behaviorMux(t, provider, CatalogModern)
	_, sessionID := listHTTPTools(t, mux, endpoint+"/mcp/modern", "key")
	params, _ := json.Marshal(map[string]any{"name": "search_events_v2", "arguments": map[string]any{"query": "meeting"}})
	request := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":` + string(params) + `}`
	resp := postMCP(t, mux, endpoint+"/mcp/modern", "key", sessionID, request)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown tools/call status = %d", resp.StatusCode)
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Error) == 0 || string(envelope.Error) == "null" {
		t.Fatalf("modern catalog did not reject old tool name: %s", envelope.Error)
	}
}

type catalogBehaviorProvider struct {
	name           string
	legacyEvents   []calendar.Event
	page           calendar.Page[calendar.EventV2]
	listRequests   []calendar.ListEventsRequestV2
	capabilities   calendar.CalendarCapabilities
	createRequests []calendar.CreateEventRequestV2
	updateRequests []calendar.UpdateEventRequestV2
	deleteRequests []calendar.DeleteEventRequestV2
	created        *calendar.EventV2
	updated        *calendar.OperationResult
}

func (p *catalogBehaviorProvider) Name() string { return p.name }
func (p *catalogBehaviorProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	return []calendar.Calendar{{ID: "primary", Provider: p.name, Name: "Primary"}}, nil
}
func (p *catalogBehaviorProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return append([]calendar.Event(nil), p.legacyEvents...), nil
}
func (p *catalogBehaviorProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return nil, nil
}
func (p *catalogBehaviorProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return nil, nil
}
func (p *catalogBehaviorProvider) DeleteEvent(context.Context, string, string) error { return nil }
func (p *catalogBehaviorProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return p.capabilities, nil
}
func (p *catalogBehaviorProvider) ListEventsV2(_ context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.listRequests = append(p.listRequests, request)
	return p.page, nil
}
func (p *catalogBehaviorProvider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	return &calendar.EventV2{ID: "event-1", CalendarID: "primary"}, nil
}
func (p *catalogBehaviorProvider) CreateEventV2(_ context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.createRequests = append(p.createRequests, request)
	created := *p.created
	created.Title = request.Event.Title
	return &created, nil
}
func (p *catalogBehaviorProvider) UpdateEventV2(_ context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updateRequests = append(p.updateRequests, request)
	copy := *p.updated
	if copy.Event != nil {
		event := *copy.Event
		copy.Event = &event
	}
	return &copy, nil
}
func (p *catalogBehaviorProvider) DeleteEventV2(_ context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.deleteRequests = append(p.deleteRequests, request)
	return &calendar.OperationResult{Status: "completed"}, nil
}

func behaviorMux(t *testing.T, provider *catalogBehaviorProvider, mode CatalogMode) (*http.ServeMux, string) {
	t.Helper()
	reg := calendar.NewRegistry([]calendar.Provider{provider})
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "key"}, Options{EnableV2: true, CatalogMode: mode}); err != nil {
		t.Fatal(err)
	}
	return mux, "http://catalog.test"
}

func resultText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tool result has no content: %#v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("tool content has unexpected shape: %#v", content[0])
	}
	text, ok := first["text"].(string)
	if !ok {
		t.Fatalf("tool content has no text: %#v", first)
	}
	return text
}
