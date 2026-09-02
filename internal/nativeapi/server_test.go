package nativeapi

import (
	"context"
	"encoding/json"
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
}

func (*testProvider) Name() string                                               { return "google" }
func (*testProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) { return nil, nil }
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
	if response.Sources[0] != (cachedSource{CalendarID: "google:primary", Status: "ready", Stale: false}) {
		t.Fatalf("ready source = %#v", response.Sources[0])
	}
	if response.Sources[1] != (cachedSource{CalendarID: "google:unwarmed", Status: "pending", Stale: true}) {
		t.Fatalf("pending source = %#v", response.Sources[1])
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
