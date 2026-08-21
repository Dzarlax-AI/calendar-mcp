package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type uiAPIProvider struct {
	name               string
	capabilities       calendar.CalendarCapabilities
	events             []calendar.EventV2
	lastCreate         calendar.CreateEventRequestV2
	lastUpdate         calendar.UpdateEventRequestV2
	lastDelete         calendar.DeleteEventRequestV2
	lastGet            calendar.EventRef
	createCalls        int
	updateCalls        int
	deleteCalls        int
	listCalls          []calendar.ListEventsRequestV2
	pages              map[string]calendar.Page[calendar.EventV2]
	listErrors         map[string]error
	nextPageToken      string
	paginateUntilLimit bool
}

func (p *uiAPIProvider) Name() string { return p.name }
func (p *uiAPIProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	return []calendar.Calendar{{ID: "primary", Name: "Primary"}}, nil
}
func (p *uiAPIProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (p *uiAPIProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return nil, errors.New("legacy API must not be used")
}
func (p *uiAPIProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return nil, errors.New("legacy API must not be used")
}
func (p *uiAPIProvider) DeleteEvent(context.Context, string, string) error {
	return errors.New("legacy API must not be used")
}
func (p *uiAPIProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return p.capabilities, nil
}
func (p *uiAPIProvider) ListEventsV2(_ context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.listCalls = append(p.listCalls, request)
	key := uiPageKey(request)
	if err, ok := p.listErrors[key]; ok {
		return calendar.Page[calendar.EventV2]{}, err
	}
	if page, ok := p.pages[key]; ok {
		return page, nil
	}
	if request.CalendarID == "broken" {
		return calendar.Page[calendar.EventV2]{}, errors.New("provider credential-token-should-not-leak failed")
	}
	nextPageToken := p.nextPageToken
	if p.paginateUntilLimit {
		nextPageToken = strings.Repeat("n", len(p.listCalls))
	}
	return calendar.Page[calendar.EventV2]{Items: append([]calendar.EventV2(nil), p.events...), NextPageToken: nextPageToken, Complete: true}, nil
}
func (p *uiAPIProvider) GetEventV2(_ context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	p.lastGet = ref
	if len(p.events) == 0 {
		return nil, calendar.NewAPIError(calendar.ErrorNotFound, "not found")
	}
	event := p.events[0]
	event.ID = ref.EventID
	return &event, nil
}
func (p *uiAPIProvider) CreateEventV2(_ context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.createCalls++
	p.lastCreate = request
	return &calendar.EventV2{ID: "created", CalendarID: request.CalendarID, Title: request.Event.Title, Start: request.Event.Start, End: request.Event.End}, nil
}
func (p *uiAPIProvider) UpdateEventV2(_ context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updateCalls++
	p.lastUpdate = request
	return &calendar.OperationResult{Status: "completed"}, nil
}
func (p *uiAPIProvider) DeleteEventV2(_ context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.deleteCalls++
	p.lastDelete = request
	return &calendar.OperationResult{Status: "completed"}, nil
}

func uiPageKey(request calendar.ListEventsRequestV2) string {
	return request.CalendarID + "\x00" + request.PageToken
}

func newUIAPIHandler(t *testing.T, cfg Config, provider *uiAPIProvider) http.Handler {
	handler, _ := newUIAPIHandlerWithStore(t, cfg, provider)
	return handler
}

func newUIAPIHandlerWithStore(t *testing.T, cfg Config, provider *uiAPIProvider) (http.Handler, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateConnection(ctx, storage.Connection{ID: "account", Provider: provider.name, AccountFingerprint: "account", DisplayName: "Personal account", Status: "connected", EncryptedCredentials: []byte("credential-secret"), CredentialVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(ctx, storage.Calendar{ID: provider.name + ":primary", ConnectionID: "account", ProviderCalendarID: "primary", Name: "Primary", Timezone: "Europe/Belgrade", CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	registry := calendar.NewRegistry([]calendar.Provider{provider})
	cfg.ApplicationService = application.New(registry)
	if cfg.PublicURL == "" {
		cfg.PublicURL = "https://calendar.example"
	}
	if cfg.AppleCalDAVURL == "" {
		cfg.AppleCalDAVURL = "https://caldav.icloud.com"
	}
	server, err := New(store, connections.New(store, cipher), nil, func(context.Context) ([]calendar.Provider, error) { return []calendar.Provider{provider}, nil }, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler(), store
}

func uiProvider() *uiAPIProvider {
	return &uiAPIProvider{name: "google", capabilities: calendar.CalendarCapabilities{
		Operations:           calendar.OperationCapabilities{List: true, Get: true, Create: true, Update: true, Delete: true},
		MutationScopes:       []calendar.MutationScope{calendar.ScopeSingle, calendar.ScopeSeries, calendar.ScopeFollowing},
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
	}}
}

func TestUIAPIBootstrapIsProtectedSafeAndFlat(t *testing.T) {
	provider := uiProvider()
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true, MCPAPIKey: "primary-secret", LegacyAPIKeyConfigured: true}, provider)

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/bootstrap", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticated.Code)
	}
	if !strings.Contains(unauthenticated.Body.String(), `"code":"permission_denied"`) {
		t.Fatalf("unauthenticated response = %s", unauthenticated.Body.String())
	}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/bootstrap", nil)
	request.Header.Set("X-authentik-username", "alexey")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"primary-secret", "credential-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("bootstrap leaks %q", secret)
		}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"csrf_token", "calendars", "capabilities", "connections", "rules", "runs", "settings", "mcp_endpoint", "legacy_api_key_configured"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("bootstrap lacks top-level %q: %s", key, response.Body.String())
		}
	}
	if _, ok := payload["control_plane"]; ok {
		t.Fatal("bootstrap should remain flat")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("csrf cookie = %#v, want Secure cookie for HTTPS public URL", cookies)
	}
	assertNoStoreHeaders(t, response)
}

func TestUIAPIBootstrapOmitsCalendarsFromInactiveConnections(t *testing.T) {
	provider := uiProvider()
	handler, store := newUIAPIHandlerWithStore(t, Config{TrustForwardAuth: true}, provider)
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	if err := store.CreateConnection(t.Context(), storage.Connection{
		ID: "pending-account", Provider: "google", DisplayName: "Pending account", Status: "pending",
		EncryptedCredentials: []byte("pending-credential"), CredentialVersion: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(t.Context(), storage.Calendar{
		ID: "google:pending-account:stale", ConnectionID: "pending-account", ProviderCalendarID: "stale",
		Name: "Stale", CanRead: true, CanWrite: true, DiscoveredAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/bootstrap", nil)
	request.Header.Set("X-authentik-username", "alexey")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Calendars []uiCalendar `json:"calendars"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Calendars) != 1 || payload.Calendars[0].ID != "google:primary" {
		t.Fatalf("bootstrap calendars = %#v, want only the connected calendar", payload.Calendars)
	}
}

func TestUIAPIEventMutationsRequireJSONCSRFAndForceNone(t *testing.T) {
	provider := uiProvider()
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)
	body := `{"calendar_id":"google:primary","title":"Synthetic","start":{"date_time":"2026-08-22T09:00:00+02:00","time_zone":"Europe/Belgrade"},"end":{"date_time":"2026-08-22T10:00:00+02:00","time_zone":"Europe/Belgrade"}}`

	missingCSRF := newUIJSONRequest(t, http.MethodPost, "https://calendar.example/api/ui/events", body, "")
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status = %d", missingResponse.Code)
	}
	evilOrigin := newUIJSONRequest(t, http.MethodPost, "https://calendar.example/api/ui/events", body, "token")
	evilOrigin.Header.Set("Origin", "https://evil.example")
	evilOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(evilOriginResponse, evilOrigin)
	if evilOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("evil origin status = %d", evilOriginResponse.Code)
	}

	unsafe := newUIJSONRequest(t, http.MethodPost, "https://calendar.example/api/ui/events", strings.Replace(body, `"title":"Synthetic",`, `"title":"Synthetic","attendees":[{"email":"guest@example.com"}],`, 1), "token")
	unsafeResponse := httptest.NewRecorder()
	handler.ServeHTTP(unsafeResponse, unsafe)
	if unsafeResponse.Code != http.StatusBadRequest || !strings.Contains(unsafeResponse.Body.String(), `"code":"invalid_argument"`) {
		t.Fatalf("unsafe response = %d %s", unsafeResponse.Code, unsafeResponse.Body.String())
	}
	if provider.createCalls != 0 {
		t.Fatal("unsafe mutation reached provider")
	}

	valid := newUIJSONRequest(t, http.MethodPost, "https://calendar.example/api/ui/events", body, "token")
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, valid)
	if validResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", validResponse.Code, validResponse.Body.String())
	}
	if provider.lastCreate.Notifications != calendar.NotificationsNone {
		t.Fatalf("notification policy = %q", provider.lastCreate.Notifications)
	}
	if strings.Contains(validResponse.Body.String(), "notification_policy") {
		t.Fatalf("browser response exposes notification policy: %s", validResponse.Body.String())
	}

	patch := newUIJSONRequest(t, http.MethodPatch, "https://calendar.example/api/ui/event?calendar_id=google%3Aprimary&event_id=google%3Aevent-1", `{"scope":"single","expected_etag":"etag-1","title":"Changed"}`, "token")
	patchResponse := httptest.NewRecorder()
	handler.ServeHTTP(patchResponse, patch)
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("patch status = %d: %s", patchResponse.Code, patchResponse.Body.String())
	}
	if provider.lastUpdate.Notifications != calendar.NotificationsNone || provider.lastUpdate.Scope != calendar.ScopeSingle || provider.lastUpdate.ExpectedETag != "etag-1" || !provider.lastUpdate.Patch.Title.Present {
		t.Fatalf("update request = %#v", provider.lastUpdate)
	}

	deleteRequest := newUIJSONRequest(t, http.MethodDelete, "https://calendar.example/api/ui/event?calendar_id=google%3Aprimary&event_id=google%3Aevent-1", `{"scope":"series","expected_etag":"etag-2"}`, "token")
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if provider.lastDelete.Notifications != calendar.NotificationsNone || provider.lastDelete.Scope != calendar.ScopeSeries || provider.lastDelete.ExpectedETag != "etag-2" {
		t.Fatalf("delete request = %#v", provider.lastDelete)
	}
}

func TestUIAPIRejectsMalformedOptionalMutationStrings(t *testing.T) {
	provider := uiProvider()
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)
	for name, body := range map[string]string{
		"scope":         `{"scope":123,"title":"Changed"}`,
		"expected etag": `{"scope":"single","expected_etag":123,"title":"Changed"}`,
		"operation ID":  `{"scope":"single","operation_id":123,"title":"Changed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := newUIJSONRequest(t, http.MethodPatch, "https://calendar.example/api/ui/event?calendar_id=google%3Aprimary&event_id=google%3Aevent-1", body, "token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
		})
	}
	if provider.updateCalls != 0 {
		t.Fatalf("malformed mutation reached provider %d times", provider.updateCalls)
	}
}

func TestUIAPICreatePreservesTimedAndAllDayEventTimes(t *testing.T) {
	provider := uiProvider()
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)
	for name, input := range map[string]struct {
		body, wantStart, wantEnd string
	}{
		"timed": {
			body:      `{"calendar_id":"google:primary","title":"Timed","start":{"date_time":"2026-08-22T09:00:00+02:00","time_zone":"Europe/Belgrade"},"end":{"date_time":"2026-08-22T10:00:00+02:00","time_zone":"Europe/Belgrade"}}`,
			wantStart: "2026-08-22T09:00:00+02:00", wantEnd: "2026-08-22T10:00:00+02:00",
		},
		"all day": {
			body:      `{"calendar_id":"google:primary","title":"All day","start":{"date":"2026-08-22"},"end":{"date":"2026-08-23"}}`,
			wantStart: "2026-08-22", wantEnd: "2026-08-23",
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := newUIJSONRequest(t, http.MethodPost, "https://calendar.example/api/ui/events", input.body, "token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusCreated {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			if got := provider.lastCreate.Event.Start.Date + provider.lastCreate.Event.Start.DateTime; got != input.wantStart {
				t.Fatalf("start = %q, want %q", got, input.wantStart)
			}
			if got := provider.lastCreate.Event.End.Date + provider.lastCreate.Event.End.DateTime; got != input.wantEnd {
				t.Fatalf("end = %q, want %q", got, input.wantEnd)
			}
		})
	}
}

func TestUIAPIRejectsOversizedJSONBody(t *testing.T) {
	provider := uiProvider()
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)
	body := `{"calendar_id":"google:primary","title":"` + strings.Repeat("a", maxUIRequestBodyBytes) + `","start":{"date":"2026-08-22"},"end":{"date":"2026-08-23"}}`
	request := newUIJSONRequest(t, http.MethodPost, "https://calendar.example/api/ui/events", body, "token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if provider.createCalls != 0 {
		t.Fatal("oversized request reached provider")
	}
}

func TestUIAPIEventRoutesAreSafeAndSupportPartialSources(t *testing.T) {
	provider := uiProvider()
	provider.events = []calendar.EventV2{{ID: "event-1", CalendarID: "primary", Title: "Private", Start: calendar.EventTime{DateTime: "2026-08-22T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T10:00:00Z", TimeZone: "UTC"}, Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "guest-secret@example.com"}}}, Reminders: &calendar.ReminderSettings{UseDefault: true}}}
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)

	get := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/event?calendar_id=google%3Aprimary&event_id=google%3Aevent-1", nil)
	get.Header.Set("X-authentik-username", "alexey")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", getResponse.Code, getResponse.Body.String())
	}
	if provider.lastGet.CalendarID != "primary" || provider.lastGet.EventID != "event-1" {
		t.Fatalf("parsed ref = %#v", provider.lastGet)
	}
	for _, forbidden := range []string{"attendees", "guest-secret@example.com", "reminders"} {
		if strings.Contains(getResponse.Body.String(), forbidden) {
			t.Fatalf("event response leaks %q: %s", forbidden, getResponse.Body.String())
		}
	}

	list := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/events?start=2026-08-22T00:00:00Z&end=2026-08-23T00:00:00Z&calendar_id=google:primary&calendar_id=google:broken", nil)
	list.Header.Set("X-authentik-username", "alexey")
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", listResponse.Code, listResponse.Body.String())
	}
	if !strings.Contains(listResponse.Body.String(), `"complete":false`) || !strings.Contains(listResponse.Body.String(), `"calendar_id":"google:broken"`) {
		t.Fatalf("partial response = %s", listResponse.Body.String())
	}
	if strings.Contains(listResponse.Body.String(), "credential-token-should-not-leak") {
		t.Fatalf("partial response leaks provider error: %s", listResponse.Body.String())
	}
	var partial uiEventsResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &partial); err != nil {
		t.Fatal(err)
	}
	if len(partial.Items) != 1 || partial.Items[0].ID != "google:event-1" {
		t.Fatalf("partial response dropped healthy event: %#v", partial.Items)
	}
}

func TestUIAPIEventListKeepsEarlierPagesWhenLaterPageFails(t *testing.T) {
	provider := uiProvider()
	provider.pages = map[string]calendar.Page[calendar.EventV2]{
		"primary\x00": {Items: []calendar.EventV2{{ID: "healthy", Title: "Healthy", Start: calendar.EventTime{DateTime: "2026-08-22T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T10:00:00Z", TimeZone: "UTC"}}}, NextPageToken: "next", Complete: true},
	}
	provider.listErrors = map[string]error{"primary\x00next": errors.New("provider secret must not leak")}
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/events?start=2026-08-22T00:00:00Z&end=2026-08-23T00:00:00Z&calendar_id=google:primary", nil)
	request.Header.Set("X-authentik-username", "alexey")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, want := range []string{`"id":"google:healthy"`, `"complete":false`, `"provider":"google"`} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("response lacks %s: %s", want, response.Body.String())
		}
	}
	if strings.Contains(response.Body.String(), "provider secret") {
		t.Fatalf("response leaks provider error: %s", response.Body.String())
	}
}

func TestUIAPIEventListDrainsPagesAndRejectsRepeatedTokens(t *testing.T) {
	t.Run("drains all pages", func(t *testing.T) {
		provider := uiProvider()
		provider.pages = map[string]calendar.Page[calendar.EventV2]{
			"primary\x00":       {Items: []calendar.EventV2{{ID: "first", Start: calendar.EventTime{DateTime: "2026-08-22T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T10:00:00Z", TimeZone: "UTC"}}}, NextPageToken: "second", Complete: true},
			"primary\x00second": {Items: []calendar.EventV2{{ID: "second", Start: calendar.EventTime{DateTime: "2026-08-22T11:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T12:00:00Z", TimeZone: "UTC"}}}, Complete: true},
		}
		response := listUIEventsResponse(t, newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider), "calendar_id=google:primary")
		if !strings.Contains(response, `"id":"google:first"`) || !strings.Contains(response, `"id":"google:second"`) || strings.Contains(response, `"complete":false`) {
			t.Fatalf("drained response = %s", response)
		}
	})

	t.Run("preserves items and reports repeated token", func(t *testing.T) {
		provider := uiProvider()
		provider.pages = map[string]calendar.Page[calendar.EventV2]{
			"primary\x00":       {Items: []calendar.EventV2{{ID: "first", Start: calendar.EventTime{DateTime: "2026-08-22T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T10:00:00Z", TimeZone: "UTC"}}}, NextPageToken: "repeat", Complete: true},
			"primary\x00repeat": {NextPageToken: "repeat", Complete: true},
		}
		response := listUIEventsResponse(t, newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider), "calendar_id=google:primary")
		if !strings.Contains(response, `"id":"google:first"`) || !strings.Contains(response, `"complete":false`) || strings.Contains(response, "repeated a page token") {
			t.Fatalf("repeated-token response = %s", response)
		}
	})
}

func TestUIAPIEventListStopsAtPageSafetyLimit(t *testing.T) {
	provider := uiProvider()
	provider.paginateUntilLimit = true
	response := listUIEventsResponse(t, newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider), "calendar_id=google:primary")
	if !strings.Contains(response, `"complete":false`) || len(provider.listCalls) != maxUIEventPages || strings.Contains(response, "browser safety limit") {
		t.Fatalf("safety-limit response = %s; calls = %d", response, len(provider.listCalls))
	}
}

func listUIEventsResponse(t *testing.T, handler http.Handler, query string) string {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/events?start=2026-08-22T00:00:00Z&end=2026-08-23T00:00:00Z&"+query, nil)
	request.Header.Set("X-authentik-username", "alexey")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func TestUIEventResponseSortsTimedEventsByInstant(t *testing.T) {
	response := uiEventsResponse{Items: []uiEvent{
		{ID: "later", CalendarID: "google:primary", Start: calendar.EventTime{DateTime: "2026-08-22T09:00:00+02:00", TimeZone: "Europe/Belgrade"}},
		{ID: "earlier", CalendarID: "google:primary", Start: calendar.EventTime{DateTime: "2026-08-22T08:30:00+03:00", TimeZone: "Europe/Moscow"}},
	}}
	response.sort()
	if response.Items[0].ID != "earlier" {
		t.Fatalf("first event = %q, want earlier instant", response.Items[0].ID)
	}
	response.Items = []uiEvent{
		{ID: "timed", CalendarID: "google:primary", Start: calendar.EventTime{DateTime: "2026-08-22T00:00:00Z", TimeZone: "UTC"}},
		{ID: "all-day", CalendarID: "google:primary", Start: calendar.EventTime{Date: "2026-08-22"}},
	}
	response.sort()
	if response.Items[0].ID != "all-day" {
		t.Fatalf("first equal-instant event = %q, want all-day", response.Items[0].ID)
	}
}

func TestDrainUIEventPagesDrainsDefaultCalendarRequest(t *testing.T) {
	pages := map[string]calendar.Page[calendar.EventV2]{
		"":     {Items: []calendar.EventV2{{ID: "first"}}, NextPageToken: "next", Complete: true},
		"next": {Items: []calendar.EventV2{{ID: "second"}}, Complete: true},
	}
	var requests []calendar.ListEventsRequestV2
	page, err := drainUIEventPages(t.Context(), calendar.ListEventsRequestV2{Start: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC), View: calendar.RecurrenceExpanded}, func(_ context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
		requests = append(requests, request)
		return pages[request.PageToken], nil
	})
	if err != nil {
		t.Fatalf("drain default request: %v", err)
	}
	if len(page.Items) != 2 || len(requests) != 2 || requests[0].CalendarID != "" || requests[1].CalendarID != "" || requests[1].PageToken != "next" {
		t.Fatalf("default-calendar pagination page=%#v requests=%#v", page, requests)
	}
}

func TestSafeUIErrorDoesNotExposeProviderDetails(t *testing.T) {
	converted := safeUIError(&calendar.APIError{Code: calendar.ErrorConflict, Message: "provider secret", Provider: "private-provider", CalendarID: "private-calendar", EventID: "private-event"})
	if converted.Message != "The event changed elsewhere; refresh and try again" {
		t.Fatalf("message = %q", converted.Message)
	}
	if converted.Provider != "" || converted.CalendarID != "" || converted.EventID != "" {
		t.Fatalf("safe error preserves provider metadata: %#v", converted)
	}
}

func TestParseUIEventRangeRejectsExcessiveRange(t *testing.T) {
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/events?start=2026-01-01T00:00:00Z&end=2026-04-05T00:00:00Z", nil)
	if _, _, err := parseUIEventRange(request); err == nil {
		t.Fatal("expected oversized range error")
	}
}

func TestUIAPIEventRoutePreservesAppleSlashIDs(t *testing.T) {
	provider := uiProvider()
	provider.name = "apple"
	provider.events = []calendar.EventV2{{ID: "placeholder", CalendarID: "calendars/family/", Title: "Family"}}
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/api/ui/event?calendar_id=apple%3Acalendars%2Ffamily%2F&event_id=events%2Ffamily-dinner.ics", nil)
	request.Header.Set("X-authentik-username", "alexey")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", response.Code, response.Body.String())
	}
	if provider.lastGet.CalendarID != "calendars/family/" || provider.lastGet.EventID != "events/family-dinner.ics" {
		t.Fatalf("parsed Apple ref = %#v", provider.lastGet)
	}
}

func TestUIAPIPublicAndOAuthRoutesStayOutsideForwardAuth(t *testing.T) {
	provider := uiProvider()
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)
	for _, path := range []string{"/", "/privacy", "/terms"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example"+path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, response.Code)
		}
	}
	oauth := httptest.NewRecorder()
	handler.ServeHTTP(oauth, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://calendar.example/oauth/google/callback?error=access_denied", nil))
	if oauth.Code != http.StatusSeeOther || oauth.Header().Get("Location") != "/connections?status=oauth_rejected" {
		t.Fatalf("OAuth callback status=%d location=%q", oauth.Code, oauth.Header().Get("Location"))
	}
}

func newUIJSONRequest(t *testing.T, method, target, body, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://calendar.example")
	req.Header.Set("X-authentik-username", "alexey")
	if token != "" {
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(&http.Cookie{Name: "calendar_csrf", Value: token})
	}
	return req
}
