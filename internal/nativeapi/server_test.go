package nativeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type testProvider struct {
	createRequest calendar.CreateEventRequestV2
	updateRequest calendar.UpdateEventRequestV2
	listCalls     int
	calendarCalls int
}

func (*testProvider) Name() string { return "google" }
func (p *testProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	p.calendarCalls++
	return nil, nil
}
func (*testProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (*testProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return nil, nil
}
func (*testProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return nil, nil
}
func (*testProvider) DeleteEvent(context.Context, string, string) error { return nil }
func (*testProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{
		Operations:           calendar.OperationCapabilities{List: true, Create: true, Update: true},
		MutationScopes:       []calendar.MutationScope{calendar.ScopeSingle},
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
	}, nil
}
func (p *testProvider) ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.listCalls++
	return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "event-1", CalendarID: "google:primary", Provider: "google", Title: "Planning", Start: calendar.EventTime{DateTime: "2026-08-28T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-28T10:00:00Z", TimeZone: "UTC"}}}, Complete: true}, nil
}
func (*testProvider) GetEventV2(_ context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	return &calendar.EventV2{ID: ref.EventID, CalendarID: ref.CalendarID, Provider: "google", ETag: "created-etag", Start: calendar.EventTime{DateTime: "2026-08-29T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-29T10:00:00Z", TimeZone: "UTC"}}, nil
}
func (p *testProvider) CreateEventV2(_ context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.createRequest = request
	return &calendar.EventV2{ID: "created", CalendarID: request.CalendarID, Provider: "google", ETag: "created-etag", Title: request.Event.Title, Description: request.Event.Description, Location: request.Event.Location, Start: request.Event.Start, End: request.Event.End}, nil
}
func (p *testProvider) UpdateEventV2(_ context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updateRequest = request
	if request.ExpectedETag == "conflict" {
		return nil, calendar.NewAPIError(calendar.ErrorConflict, "event ETag does not match expected_etag")
	}
	if request.ExpectedETag == "rate-limited" {
		return nil, calendar.NewAPIError(calendar.ErrorRateLimited, "provider rate limit reached")
	}
	return &calendar.OperationResult{Status: "updated", Event: &calendar.EventV2{ID: request.Ref.EventID, CalendarID: request.Ref.CalendarID, Provider: "google", ETag: "updated-etag"}}, nil
}
func (*testProvider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	return nil, nil
}

func testHandler(t *testing.T, writesEnabled bool) (http.Handler, *testProvider, *storage.Store) {
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
	now := time.Now().UTC()
	if err := store.CreateConnection(ctx, storage.Connection{ID: "account", Provider: "google", AccountFingerprint: "account", DisplayName: "Personal", Status: "connected", EncryptedCredentials: []byte("test"), CredentialVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCalendar(ctx, storage.Calendar{ID: "google:primary", ConnectionID: "account", ProviderCalendarID: "primary", Name: "Primary", Timezone: "UTC", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	provider := &testProvider{}
	app := application.New(calendar.NewRegistry([]calendar.Provider{provider}))
	cachedEventsApp := app.CloneWithEventReadModel(store, storage.SyncWindow{Start: now.AddDate(0, 0, -7), End: now.AddDate(0, 0, 7)})
	return New(Config{App: app, CachedEventsApp: cachedEventsApp, Store: store, Token: "native-token", ReadOnlyToken: "read-only-token", WritesEnabled: writesEnabled}).Handler(), provider, store
}

func request(handler http.Handler, method, path, token string, body ...string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if len(body) > 0 {
		reader = strings.NewReader(body[0])
	} else {
		reader = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, reader)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestNativeAPIRequiresDedicatedToken(t *testing.T) {
	handler, _, _ := testHandler(t, false)
	if got := request(handler, http.MethodGet, "/bootstrap", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request(handler, http.MethodGet, "/bootstrap", "wrong").Code; got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request(handler, http.MethodGet, "/bootstrap", "native-token").Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	if got := request(handler, http.MethodGet, "/cached-events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:00Z", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("cached events status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request(handler, http.MethodGet, "/cached-events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:00Z", "native-token").Code; got != http.StatusUnauthorized {
		t.Fatalf("cached events with native token status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request(handler, http.MethodGet, "/cached-events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:00Z", "read-only-token").Code; got != http.StatusOK {
		t.Fatalf("cached events with read-only token status = %d, want %d", got, http.StatusOK)
	}
	if got := request(handler, http.MethodPost, "/events", "read-only-token", `{}`).Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("write with read-only token status = %d, want %d", got, http.StatusMethodNotAllowed)
	}
}

func TestReadOnlyCalendarCatalog(t *testing.T) {
	handler, provider, store := testHandler(t, true)
	now := time.Now().UTC()
	for _, item := range []storage.Calendar{
		{ID: "google:unicode", ConnectionID: "account", ProviderCalendarID: "unicode", Name: "Календарь", Timezone: "Europe/Belgrade", CanRead: true, DiscoveredAt: now},
		{ID: "google:first", ConnectionID: "account", ProviderCalendarID: "first", Name: "Alpha", CanRead: true, CanWrite: true, DiscoveredAt: now},
		{ID: "google:hidden", ConnectionID: "account", ProviderCalendarID: "hidden", Name: "Hidden", CanRead: false, CanWrite: true, DiscoveredAt: now},
	} {
		if err := store.UpsertCalendar(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	for _, token := range []string{"", "wrong", "native-token", "mcp-token"} {
		if got := request(handler, http.MethodGet, "/calendars", token).Code; got != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d", token, got)
		}
	}
	if got := request(handler, http.MethodPost, "/calendars", "read-only-token").Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", got)
	}
	if got := request(handler, http.MethodPost, "/events", "read-only-token", `{}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("write status = %d", got)
	}
	if got := request(handler, http.MethodGet, "/bootstrap", "read-only-token").Code; got != http.StatusUnauthorized {
		t.Fatalf("bootstrap status = %d", got)
	}
	w := request(handler, http.MethodGet, "/calendars", "read-only-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var response struct {
		Calendars []calendarResponse `json:"calendars"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Calendars) != 3 || response.Calendars[0].Name != "Alpha" || response.Calendars[1].Name != "Primary" || response.Calendars[2].Name != "Календарь" || response.Calendars[2].TimeZone != "Europe/Belgrade" {
		t.Fatalf("catalog = %#v", response.Calendars)
	}
	for _, item := range response.Calendars {
		if !item.CanRead || item.CanWrite || !item.ReadOnly {
			t.Fatalf("permissions = %#v", item)
		}
	}
	if provider.calendarCalls != 0 || provider.listCalls != 0 || provider.createRequest.CalendarID != "" || provider.updateRequest.Ref.CalendarID != "" {
		t.Fatal("catalog invoked provider")
	}
	if got := New(Config{Store: store, Token: "native-token"}).Handler(); request(got, http.MethodGet, "/calendars", "native-token").Code != http.StatusNotFound {
		t.Fatal("catalog enabled without read-only token")
	}
	if got := request(New(Config{ReadOnlyToken: "read-only-token"}).Handler(), http.MethodGet, "/calendars", "read-only-token").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("missing store status = %d", got)
	}
}

func TestReadOnlyCalendarCatalogLimit(t *testing.T) {
	handler, _, store := testHandler(t, false)
	now := time.Now().UTC()
	for i := 0; i < 1999; i++ {
		id := fmt.Sprintf("google:test-%04d", i)
		if err := store.UpsertCalendar(t.Context(), storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if got := request(handler, http.MethodGet, "/calendars", "read-only-token").Code; got != http.StatusOK {
		t.Fatalf("2000 entries status = %d", got)
	}
	if err := store.UpsertCalendar(t.Context(), storage.Calendar{ID: "google:overflow", ConnectionID: "account", ProviderCalendarID: "overflow", Name: "overflow", CanRead: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	w := request(handler, http.MethodGet, "/calendars", "read-only-token")
	if w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != `{"error":"catalog_too_large"}` {
		t.Fatalf("overflow status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestNativeAPICachedEventsUsesProjectionAndReportsPartialFreshness(t *testing.T) {
	handler, provider, store := testHandler(t, false)
	prepareCachedCalendar(t, store, "google:primary")
	now := time.Now().UTC()
	if err := store.UpsertCalendar(t.Context(), storage.Calendar{ID: "google:unwarmed", ConnectionID: "account", ProviderCalendarID: "unwarmed", Name: "Unwarmed", Timezone: "UTC", CanRead: true, CanWrite: false, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []calendar.EventV2{
		{ID: "timed", CalendarID: "google:primary", Provider: "google", Title: "Planning", Description: "must not leave the cache endpoint", Status: "confirmed", Start: calendar.EventTime{DateTime: "2026-08-28T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-28T10:00:00Z", TimeZone: "UTC"}},
		{ID: "all-day", CalendarID: "google:primary", Provider: "google", Title: "Holiday", Start: calendar.EventTime{Date: "2026-08-28"}, End: calendar.EventTime{Date: "2026-08-29"}},
	} {
		if err := store.UpsertCachedEvent(t.Context(), event, now); err != nil {
			t.Fatal(err)
		}
	}

	w := request(handler, http.MethodGet, "/cached-events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:00Z&calendar_id=google:primary&calendar_id=google:unwarmed", "read-only-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if provider.listCalls != 0 {
		t.Fatalf("cached endpoint called provider ListEventsV2 %d times", provider.listCalls)
	}
	if strings.Contains(w.Body.String(), "description") || strings.Contains(w.Body.String(), "time_zone") || strings.Contains(w.Body.String(), "provider") {
		t.Fatalf("cached response exposes fields outside the narrow DTO: %s", w.Body.String())
	}
	var response cachedEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Complete || len(response.Items) != 2 || len(response.Sources) != 2 {
		t.Fatalf("response = %#v, body = %s", response, w.Body.String())
	}
	if response.Items[0].Start.Date != "2026-08-28" || response.Items[0].End.Date != "2026-08-29" {
		t.Fatalf("all-day event = %#v", response.Items[0])
	}
	if response.Items[1].Start.DateTime != "2026-08-28T09:00:00Z" || response.Items[1].End.DateTime != "2026-08-28T10:00:00Z" {
		t.Fatalf("timed event = %#v", response.Items[1])
	}
	if source := response.Sources[0]; source.CalendarID != "google:primary" || source.Status != "ready" || source.Stale || source.LastSuccessAt == nil || source.NextSyncAt == nil || source.CoverageComplete == nil {
		t.Fatalf("ready source = %#v", response.Sources[0])
	}
	if source := response.Sources[1]; source.CalendarID != "google:unwarmed" || source.Status != "pending" || !source.Stale || source.LastSuccessAt != nil || source.NextSyncAt != nil || source.CoverageComplete != nil {
		t.Fatalf("pending source = %#v", response.Sources[1])
	}
}

func TestNativeCachedSourceErrorCodeIsAllowlisted(t *testing.T) {
	for _, code := range []string{"transient", "rate_limited", "auth", "permission", "unsupported", "protocol"} {
		if got := safeNativeSyncErrorCode(code); got != code {
			t.Fatalf("safeNativeSyncErrorCode(%q) = %q", code, got)
		}
	}
	for _, code := range []string{"provider_down", "https://private.example/token", "raw error"} {
		if got := safeNativeSyncErrorCode(code); got != "" {
			t.Fatalf("unsafe error code %q escaped as %q", code, got)
		}
	}
}

func TestNativeAPICachedEventsIsIncompleteOutsideProjectionWindow(t *testing.T) {
	handler, _, store := testHandler(t, false)
	prepareCachedCalendar(t, store, "google:primary")
	w := request(handler, http.MethodGet, "/cached-events?start=2030-08-28T00:00:00Z&end=2030-08-29T00:00:00Z&calendar_id=google:primary", "read-only-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var response cachedEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Complete {
		t.Fatalf("response outside projection window must be incomplete: %#v", response)
	}
	if source := response.Sources[0]; source.CoverageComplete == nil || *source.CoverageComplete {
		t.Fatalf("coverage outside the stored window = %#v, want false", source.CoverageComplete)
	}
}

func TestNativeAPICachedSourceProjectsWindowBoundaryTimesAndSafeError(t *testing.T) {
	handler, provider, store := testHandler(t, false)
	window := storage.SyncWindow{Start: mustNativeTime(t, "2026-08-28T00:00:00Z"), End: mustNativeTime(t, "2026-08-29T00:00:00Z")}
	expected := prepareCachedCalendarWithWindow(t, store, "google:primary", window, "protocol")
	path := "/cached-events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:00Z&calendar_id=google:primary"
	w := request(handler, http.MethodGet, path, "read-only-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var response cachedEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if provider.listCalls != 0 {
		t.Fatalf("cached diagnostics called provider ListEventsV2 %d times", provider.listCalls)
	}
	if len(response.Sources) != 1 {
		t.Fatalf("sources = %#v", response.Sources)
	}
	source := response.Sources[0]
	if source.Status != "degraded" || source.ErrorCode != "protocol" || source.LastSuccessAt == nil || source.NextSyncAt == nil {
		t.Fatalf("diagnostic source = %#v", source)
	}
	if *source.LastSuccessAt != expected.LastSuccessAt.UTC().Format(time.RFC3339) || *source.NextSyncAt != expected.NextSyncAt.UTC().Format(time.RFC3339) {
		t.Fatalf("UTC timestamps = last:%q next:%q, expected %#v", *source.LastSuccessAt, *source.NextSyncAt, expected)
	}
	if source.CoverageComplete == nil || !*source.CoverageComplete {
		t.Fatalf("exact window boundary coverage = %#v, want true", source.CoverageComplete)
	}
	outside := request(handler, http.MethodGet, "/cached-events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:01Z&calendar_id=google:primary", "read-only-token")
	if outside.Code != http.StatusOK {
		t.Fatalf("outside boundary status = %d, body = %s", outside.Code, outside.Body.String())
	}
	if err := json.Unmarshal(outside.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if source := response.Sources[0]; source.CoverageComplete == nil || *source.CoverageComplete {
		t.Fatalf("coverage past the stored boundary = %#v, want false", source.CoverageComplete)
	}
	if provider.listCalls != 0 {
		t.Fatalf("diagnostic range check called provider ListEventsV2 %d times", provider.listCalls)
	}
}

func TestNativeAPICachedEventsReturnsServiceUnavailableWithoutReadModel(t *testing.T) {
	handler, _, store := testHandler(t, false)
	provider := &testProvider{}
	app := application.New(calendar.NewRegistry([]calendar.Provider{provider}))
	handler = New(Config{App: app, Store: store, Token: "native-token", ReadOnlyToken: "read-only-token"}).Handler()
	if got := request(handler, http.MethodGet, "/cached-events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:00Z", "read-only-token").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func prepareCachedCalendar(t *testing.T, store *storage.Store, calendarID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.EnsureCalendarSyncStates(t.Context(), now, storage.SyncWindow{Start: now.AddDate(0, 0, -7), End: now.AddDate(0, 0, 7)}); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(t.Context(), "warm-worker", now, now.Add(time.Minute))
	if err != nil || state == nil || state.CalendarID != calendarID {
		t.Fatalf("claim cached calendar state=%#v err=%v", state, err)
	}
	next := now.Add(time.Hour)
	if err := store.ApplyEventSyncPage(t.Context(), *state, storage.EventSyncBatch{NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
}

func prepareCachedCalendarWithWindow(t *testing.T, store *storage.Store, calendarID string, window storage.SyncWindow, errorCode string) storage.CachedSourceStatus {
	t.Helper()
	now := time.Now().UTC()
	if err := store.EnsureCalendarSyncStates(t.Context(), now, window); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(t.Context(), "diagnostic-worker", now, now.Add(time.Minute))
	if err != nil || state == nil || state.CalendarID != calendarID {
		t.Fatalf("claim diagnostic calendar state=%#v err=%v", state, err)
	}
	next := now.Add(time.Hour)
	batch := storage.EventSyncBatch{NextSyncAt: &next}
	if errorCode != "" {
		batch.Degraded = true
		batch.ErrorCode = errorCode
	}
	if err := store.ApplyEventSyncPage(t.Context(), *state, batch, true, now); err != nil {
		t.Fatal(err)
	}
	_, statuses, err := store.ListCachedEvents(t.Context(), []string{calendarID}, window.Start, window.End)
	if err != nil || len(statuses) != 1 {
		t.Fatalf("diagnostic statuses=%#v err=%v", statuses, err)
	}
	return statuses[0]
}

func mustNativeTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestNativeAPIListsReadOnlyDataAndWritesOrdinaryEventsWithoutNotifications(t *testing.T) {
	handler, provider, _ := testHandler(t, true)
	w := request(handler, http.MethodGet, "/events?start=2026-08-28T00:00:00Z&end=2026-08-29T00:00:00Z", "native-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var response struct {
		Items    []calendar.EventV2 `json:"items"`
		Complete bool               `json:"complete"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Complete || len(response.Items) != 1 || response.Items[0].Title != "Planning" {
		t.Fatalf("response = %#v, body = %s", response, w.Body.String())
	}
	body := `{"calendar_id":"google:primary","title":"Focus time","description":"Private note","location":"Home","start":{"date_time":"2026-08-29T09:00:00Z","time_zone":"UTC"},"end":{"date_time":"2026-08-29T10:00:00Z","time_zone":"UTC"},"all_day":false}`
	if got := request(handler, http.MethodPost, "/events", "native-token", body).Code; got != http.StatusCreated {
		t.Fatalf("create status = %d", got)
	}
	if provider.createRequest.Notifications != calendar.NotificationsNone || provider.createRequest.Event.Title != "Focus time" {
		t.Fatalf("create request = %#v", provider.createRequest)
	}
	update := `{"calendar_id":"google:primary","title":"Updated focus time","description":"","location":"Desk","start":{"date_time":"2026-08-29T10:00:00Z","time_zone":"UTC"},"end":{"date_time":"2026-08-29T11:00:00Z","time_zone":"UTC"},"all_day":false,"expected_etag":"created-etag"}`
	if got := request(handler, http.MethodPatch, "/events/google:primary/created", "native-token", update).Code; got != http.StatusOK {
		t.Fatalf("update status = %d", got)
	}
	if provider.updateRequest.Notifications != calendar.NotificationsNone || provider.updateRequest.Scope != calendar.ScopeSingle || provider.updateRequest.ExpectedETag != "created-etag" {
		t.Fatalf("update request = %#v", provider.updateRequest)
	}
	conflict := `{"calendar_id":"google:primary","title":"Updated focus time","description":"","location":"Desk","start":{"date_time":"2026-08-29T10:00:00Z","time_zone":"UTC"},"end":{"date_time":"2026-08-29T11:00:00Z","time_zone":"UTC"},"all_day":false,"expected_etag":"conflict"}`
	if got := request(handler, http.MethodPatch, "/events/google:primary/created", "native-token", conflict).Code; got != http.StatusConflict {
		t.Fatalf("conflict status = %d, want %d", got, http.StatusConflict)
	}
	rateLimited := `{"title":"Updated focus time","expected_etag":"rate-limited"}`
	if got := request(handler, http.MethodPatch, "/events/google:primary/created", "native-token", rateLimited).Code; got != http.StatusTooManyRequests {
		t.Fatalf("rate-limited status = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestNativeAPIWritesRequireExplicitOptIn(t *testing.T) {
	handler, _, _ := testHandler(t, false)
	if got := request(handler, http.MethodPost, "/events", "native-token", `{}`).Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("create status = %d, want %d", got, http.StatusMethodNotAllowed)
	}
	if got := request(handler, http.MethodPatch, "/events/google:primary/event-1", "native-token", `{}`).Code; got != http.StatusNotFound {
		t.Fatalf("update status = %d, want %d", got, http.StatusNotFound)
	}
	w := request(handler, http.MethodGet, "/bootstrap", "native-token")
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, body = %s", w.Code, w.Body.String())
	}
	var response struct {
		Calendars []calendarResponse `json:"calendars"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Calendars) != 1 || response.Calendars[0].CanWrite || !response.Calendars[0].ReadOnly {
		t.Fatalf("calendars = %#v", response.Calendars)
	}
}

func TestNativeAPIPatchPreservesOmittedFields(t *testing.T) {
	handler, provider, _ := testHandler(t, true)
	if got := request(handler, http.MethodPatch, "/events/google:primary/event-1", "native-token", `{"title":"Retitled"}`).Code; got != http.StatusOK {
		t.Fatalf("update status = %d", got)
	}
	patch := provider.updateRequest.Patch
	if !patch.Title.Present || patch.Title.Value != "Retitled" {
		t.Fatalf("title patch = %#v", patch.Title)
	}
	if patch.Description.Present || patch.Location.Present || patch.Start.Present || patch.End.Present {
		t.Fatalf("omitted fields must not be patched: %#v", patch)
	}
}

func TestAppleOrdinaryUpdatesUseSeriesScopeWithoutETag(t *testing.T) {
	scope, expectedETag := ordinaryUpdateScope("apple", "existing-etag")
	if scope != calendar.ScopeSeries || expectedETag != "" {
		t.Fatalf("scope = %q, expected_etag = %q", scope, expectedETag)
	}
}

func TestSortEventsByStartUsesInstantsAndPlacesAllDayFirstOnATie(t *testing.T) {
	items := []calendar.EventV2{
		{ID: "later", CalendarID: "google:primary", Start: calendar.EventTime{DateTime: "2026-08-22T09:00:00+02:00", TimeZone: "Europe/Belgrade"}},
		{ID: "earlier", CalendarID: "google:primary", Start: calendar.EventTime{DateTime: "2026-08-22T08:30:00+03:00", TimeZone: "Europe/Moscow"}},
	}
	sortEventsByStart(items)
	if items[0].ID != "earlier" {
		t.Fatalf("first event = %q, want earlier instant", items[0].ID)
	}
	items = []calendar.EventV2{
		{ID: "timed", CalendarID: "google:primary", Start: calendar.EventTime{DateTime: "2026-08-22T00:00:00Z", TimeZone: "UTC"}},
		{ID: "all-day", CalendarID: "google:primary", Start: calendar.EventTime{Date: "2026-08-22"}},
	}
	sortEventsByStart(items)
	if items[0].ID != "all-day" {
		t.Fatalf("first equal-instant event = %q, want all-day", items[0].ID)
	}
}

func TestNativeCachedCoverageUnknownUntilChangedWindowFinishes(t *testing.T) {
	handler, provider, store := testHandler(t, false)
	oldWindow := storage.SyncWindow{Start: mustNativeTime(t, "2026-08-28T00:00:00Z"), End: mustNativeTime(t, "2026-08-29T00:00:00Z")}
	prepareCachedCalendarWithWindow(t, store, "google:primary", oldWindow, "")
	now := time.Now().UTC().Add(time.Second)
	newWindow := storage.SyncWindow{Start: oldWindow.Start.Add(-24 * time.Hour), End: oldWindow.End.Add(24 * time.Hour)}
	if err := store.EnsureCalendarSyncStates(t.Context(), now, newWindow); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"pending", "failed"} {
		if expected == "failed" {
			claim, err := store.ClaimDueCalendarSync(t.Context(), "changed-window-worker", now, now.Add(time.Minute))
			if err != nil || claim == nil {
				t.Fatalf("claim=%#v err=%v", claim, err)
			}
			if err := store.FailCalendarSync(t.Context(), *claim, "transient", now.Add(time.Second), now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		w := request(handler, http.MethodGet, "/cached-events?start=2026-08-27T00:00:00Z&end=2026-08-28T00:00:00Z&calendar_id=google:primary", "read-only-token")
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d", w.Code)
		}
		var response cachedEventsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Sources) != 1 {
			t.Fatalf("sources=%#v", response.Sources)
		}
		source := response.Sources[0]
		if response.Complete || source.Status != expected || source.LastSuccessAt == nil || source.CoverageComplete != nil {
			t.Fatalf("changed window falsely considered synchronized: complete=%t source=%#v", response.Complete, source)
		}
	}
	if provider.listCalls != 0 {
		t.Fatalf("read-only diagnostics invoked provider %d times", provider.listCalls)
	}
}
