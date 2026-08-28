package nativeapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type testProvider struct{}

func (testProvider) Name() string                                               { return "google" }
func (testProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) { return nil, nil }
func (testProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (testProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return nil, nil
}
func (testProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return nil, nil
}
func (testProvider) DeleteEvent(context.Context, string, string) error { return nil }
func (testProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{Operations: calendar.OperationCapabilities{List: true}}, nil
}
func (testProvider) ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "event-1", CalendarID: "google:primary", Provider: "google", Title: "Planning", Start: calendar.EventTime{DateTime: "2026-08-28T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-28T10:00:00Z", TimeZone: "UTC"}}}, Complete: true}, nil
}
func (testProvider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	return nil, nil
}
func (testProvider) CreateEventV2(context.Context, calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	return nil, nil
}
func (testProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	return nil, nil
}
func (testProvider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	return nil, nil
}

func testHandler(t *testing.T) http.Handler {
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
	app := application.New(calendar.NewRegistry([]calendar.Provider{testProvider{}}))
	return New(Config{App: app, Store: store, Token: "native-token"}).Handler()
}

func request(handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestNativeAPIRequiresDedicatedToken(t *testing.T) {
	handler := testHandler(t)
	if got := request(handler, http.MethodGet, "/bootstrap", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request(handler, http.MethodGet, "/bootstrap", "wrong").Code; got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request(handler, http.MethodGet, "/bootstrap", "native-token").Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
}

func TestNativeAPIListsReadOnlyDataAndHasNoMutationRoutes(t *testing.T) {
	handler := testHandler(t)
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
	if got := request(handler, http.MethodPost, "/events", "native-token").Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("mutation status = %d, want %d", got, http.StatusMethodNotAllowed)
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
