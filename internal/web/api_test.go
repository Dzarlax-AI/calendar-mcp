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
	name         string
	capabilities calendar.CalendarCapabilities
	events       []calendar.EventV2
	lastCreate   calendar.CreateEventRequestV2
	lastUpdate   calendar.UpdateEventRequestV2
	lastDelete   calendar.DeleteEventRequestV2
	lastGet      calendar.EventRef
	createCalls  int
	listCalls    []calendar.ListEventsRequestV2
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
	if request.CalendarID == "broken" {
		return calendar.Page[calendar.EventV2]{}, errors.New("provider credential-token-should-not-leak failed")
	}
	return calendar.Page[calendar.EventV2]{Items: append([]calendar.EventV2(nil), p.events...), Complete: true}, nil
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
	p.lastUpdate = request
	return &calendar.OperationResult{Status: "completed"}, nil
}
func (p *uiAPIProvider) DeleteEventV2(_ context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.lastDelete = request
	return &calendar.OperationResult{Status: "completed"}, nil
}

func newUIAPIHandler(t *testing.T, cfg Config, provider *uiAPIProvider) http.Handler {
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
	return server.Handler()
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
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "https://calendar.example/api/ui/bootstrap", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticated.Code)
	}
	if !strings.Contains(unauthenticated.Body.String(), `"code":"permission_denied"`) {
		t.Fatalf("unauthenticated response = %s", unauthenticated.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "https://calendar.example/api/ui/bootstrap", nil)
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

func TestUIAPIEventMutationsRequireJSONCSRFAndForceNone(t *testing.T) {
	provider := uiProvider()
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)
	body := `{"calendar_id":"google:primary","title":"Synthetic","start":{"date_time":"2026-08-22T09:00:00+02:00","time_zone":"Europe/Belgrade"},"end":{"date_time":"2026-08-22T10:00:00+02:00","time_zone":"Europe/Belgrade"}}`

	missingCSRF := newUIJSONRequest(http.MethodPost, "https://calendar.example/api/ui/events", body, "")
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status = %d", missingResponse.Code)
	}
	evilOrigin := newUIJSONRequest(http.MethodPost, "https://calendar.example/api/ui/events", body, "token")
	evilOrigin.Header.Set("Origin", "https://evil.example")
	evilOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(evilOriginResponse, evilOrigin)
	if evilOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("evil origin status = %d", evilOriginResponse.Code)
	}

	unsafe := newUIJSONRequest(http.MethodPost, "https://calendar.example/api/ui/events", strings.Replace(body, `"title":"Synthetic",`, `"title":"Synthetic","attendees":[{"email":"guest@example.com"}],`, 1), "token")
	unsafeResponse := httptest.NewRecorder()
	handler.ServeHTTP(unsafeResponse, unsafe)
	if unsafeResponse.Code != http.StatusBadRequest || !strings.Contains(unsafeResponse.Body.String(), "attendees") {
		t.Fatalf("unsafe response = %d %s", unsafeResponse.Code, unsafeResponse.Body.String())
	}
	if provider.createCalls != 0 {
		t.Fatal("unsafe mutation reached provider")
	}

	valid := newUIJSONRequest(http.MethodPost, "https://calendar.example/api/ui/events", body, "token")
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

	patch := newUIJSONRequest(http.MethodPatch, "https://calendar.example/api/ui/event?calendar_id=google%3Aprimary&event_id=google%3Aevent-1", `{"scope":"single","expected_etag":"etag-1","title":"Changed"}`, "token")
	patchResponse := httptest.NewRecorder()
	handler.ServeHTTP(patchResponse, patch)
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("patch status = %d: %s", patchResponse.Code, patchResponse.Body.String())
	}
	if provider.lastUpdate.Notifications != calendar.NotificationsNone || provider.lastUpdate.Scope != calendar.ScopeSingle || provider.lastUpdate.ExpectedETag != "etag-1" || !provider.lastUpdate.Patch.Title.Present {
		t.Fatalf("update request = %#v", provider.lastUpdate)
	}

	deleteRequest := newUIJSONRequest(http.MethodDelete, "https://calendar.example/api/ui/event?calendar_id=google%3Aprimary&event_id=google%3Aevent-1", `{"scope":"series","expected_etag":"etag-2"}`, "token")
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if provider.lastDelete.Notifications != calendar.NotificationsNone || provider.lastDelete.Scope != calendar.ScopeSeries || provider.lastDelete.ExpectedETag != "etag-2" {
		t.Fatalf("delete request = %#v", provider.lastDelete)
	}
}

func TestUIAPIEventRoutesAreSafeAndSupportPartialSources(t *testing.T) {
	provider := uiProvider()
	provider.events = []calendar.EventV2{{ID: "event-1", CalendarID: "primary", Title: "Private", Start: calendar.EventTime{DateTime: "2026-08-22T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-22T10:00:00Z", TimeZone: "UTC"}, Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "guest-secret@example.com"}}}, Reminders: &calendar.ReminderSettings{UseDefault: true}}}
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)

	get := httptest.NewRequest(http.MethodGet, "https://calendar.example/api/ui/event?calendar_id=google%3Aprimary&event_id=google%3Aevent-1", nil)
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

	list := httptest.NewRequest(http.MethodGet, "https://calendar.example/api/ui/events?start=2026-08-22T00:00:00Z&end=2026-08-23T00:00:00Z&calendar_id=google:primary&calendar_id=google:broken", nil)
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
}

func TestUIAPIEventRoutePreservesAppleSlashIDs(t *testing.T) {
	provider := uiProvider()
	provider.name = "apple"
	provider.events = []calendar.EventV2{{ID: "placeholder", CalendarID: "calendars/family/", Title: "Family"}}
	handler := newUIAPIHandler(t, Config{TrustForwardAuth: true}, provider)

	request := httptest.NewRequest(http.MethodGet, "https://calendar.example/api/ui/event?calendar_id=apple%3Acalendars%2Ffamily%2F&event_id=events%2Ffamily-dinner.ics", nil)
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
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://calendar.example"+path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, response.Code)
		}
	}
	oauth := httptest.NewRecorder()
	handler.ServeHTTP(oauth, httptest.NewRequest(http.MethodGet, "https://calendar.example/oauth/google/callback?error=access_denied", nil))
	if oauth.Code != http.StatusSeeOther || oauth.Header().Get("Location") != "/connections?status=oauth_rejected" {
		t.Fatalf("OAuth callback status=%d location=%q", oauth.Code, oauth.Header().Get("Location"))
	}
}

func newUIJSONRequest(method, target, body, token string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://calendar.example")
	req.Header.Set("X-authentik-username", "alexey")
	if token != "" {
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(&http.Cookie{Name: "calendar_csrf", Value: token})
	}
	return req
}
