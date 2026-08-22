package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func TestCreateEventDefaultsNotificationsAndNormalizesIDs(t *testing.T) {
	p := &stubV2Provider{
		name: "google",
		capabilities: calendar.CalendarCapabilities{
			NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
		},
		created: &calendar.EventV2{ID: "event", CalendarID: "primary"},
	}
	service := New(calendar.NewRegistry([]calendar.Provider{p}))

	got, err := service.CreateEvent(context.Background(), calendar.CreateEventRequestV2{
		CalendarID: "google:primary",
		Event: calendar.EventCreateV2{
			Start: calendar.EventTime{DateTime: "2026-08-10T09:00:00+02:00", TimeZone: "Europe/Belgrade"},
			End:   calendar.EventTime{DateTime: "2026-08-10T10:00:00+02:00", TimeZone: "Europe/Belgrade"},
		},
	})
	if err != nil {
		t.Fatalf("CreateEvent() error = %v", err)
	}
	if p.createRequest.Notifications != calendar.NotificationsNone {
		t.Fatalf("notification policy = %q, want none", p.createRequest.Notifications)
	}
	if p.createRequest.CalendarID != "primary" {
		t.Fatalf("provider calendar ID = %q, want primary", p.createRequest.CalendarID)
	}
	if got.ID != "google:event" || got.CalendarID != "google:primary" || got.Provider != "google" {
		t.Fatalf("normalized event = %#v", got)
	}
}

func TestCreateEventRejectsUnsupportedNotificationPolicy(t *testing.T) {
	p := &stubV2Provider{name: "microsoft", capabilities: calendar.CalendarCapabilities{
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsAll},
	}}
	service := New(calendar.NewRegistry([]calendar.Provider{p}))

	_, err := service.CreateEvent(context.Background(), calendar.CreateEventRequestV2{
		CalendarID:    "microsoft:calendar",
		Notifications: calendar.NotificationsNone,
		Event: calendar.EventCreateV2{
			Start: calendar.EventTime{Date: "2026-08-10"},
			End:   calendar.EventTime{Date: "2026-08-11"},
		},
	})
	if err == nil {
		t.Fatal("CreateEvent() returned nil error for unsupported notification policy")
	}
	apiErr, ok := err.(*calendar.APIError)
	if !ok || apiErr.Code != calendar.ErrorUnsupportedCapability {
		t.Fatalf("error = %#v, want unsupported_capability", err)
	}
	if p.createCalled {
		t.Fatal("provider create was called after failed capability validation")
	}
}

func TestCreateEventWritesThroughAndWarnsWithoutRepeatingProviderMutation(t *testing.T) {
	provider := &stubV2Provider{name: "google", capabilities: calendar.CalendarCapabilities{
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
	}, created: &calendar.EventV2{ID: "event", CalendarID: "primary"}}
	model := &stubEventReadModel{}
	service := New(calendar.NewRegistry([]calendar.Provider{provider}), WithEventReadModel(model))

	event, warnings, err := service.CreateEventWithReconciliation(t.Context(), calendar.CreateEventRequestV2{
		CalendarID: "google:primary",
		Event: calendar.EventCreateV2{
			Start: calendar.EventTime{Date: "2026-08-22"}, End: calendar.EventTime{Date: "2026-08-23"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.createCalls != 1 || len(model.upserts) != 1 || len(model.scheduled) != 1 {
		t.Fatalf("provider=%d upserts=%d scheduled=%d", provider.createCalls, len(model.upserts), len(model.scheduled))
	}
	if event.ID != "google:event" || model.upserts[0].ID != "event" || model.scheduled[0] != "google:primary" || len(warnings) != 0 {
		t.Fatalf("event=%#v upserts=%#v scheduled=%#v warnings=%#v", event, model.upserts, model.scheduled, warnings)
	}

	model.upsertErr = errors.New("projection unavailable")
	model.scheduleErr = errors.New("scheduler unavailable")
	_, warnings, err = service.CreateEventWithReconciliation(t.Context(), calendar.CreateEventRequestV2{
		CalendarID: "google:primary",
		Event:      calendar.EventCreateV2{Start: calendar.EventTime{Date: "2026-08-24"}, End: calendar.EventTime{Date: "2026-08-25"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.createCalls != 2 || len(warnings) != 1 || warnings[0] != reconciliationWarning {
		t.Fatalf("provider=%d warnings=%#v", provider.createCalls, warnings)
	}
}

func TestDeleteEventWritesThroughAndReturnsReconciliationWarning(t *testing.T) {
	provider := &stubV2Provider{name: "google", capabilities: calendar.CalendarCapabilities{
		MutationScopes: []calendar.MutationScope{calendar.ScopeSeries}, NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
	}}
	model := &stubEventReadModel{deleteErr: errors.New("projection unavailable")}
	service := New(calendar.NewRegistry([]calendar.Provider{provider}), WithEventReadModel(model))
	result, err := service.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: "google:primary", EventID: "google:event"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone})
	if err != nil {
		t.Fatal(err)
	}
	if provider.deleteCalls != 1 || len(model.deleted) != 1 || len(model.scheduled) != 1 || len(result.Warnings) != 1 || result.Warnings[0] != reconciliationWarning {
		t.Fatalf("provider=%d deleted=%#v scheduled=%#v warnings=%#v", provider.deleteCalls, model.deleted, model.scheduled, result.Warnings)
	}
}

func TestUpdateEventPatchesReadModelAndSchedulesReconciliation(t *testing.T) {
	provider := &stubV2Provider{name: "google", capabilities: calendar.CalendarCapabilities{
		MutationScopes: []calendar.MutationScope{calendar.ScopeSingle}, NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
	}, updated: &calendar.OperationResult{Status: "completed", Event: &calendar.EventV2{ID: "event", CalendarID: "primary"}}}
	model := &stubEventReadModel{}
	service := New(calendar.NewRegistry([]calendar.Provider{provider}), WithEventReadModel(model))
	result, err := service.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: "google:primary", EventID: "google:event"}, Scope: calendar.ScopeSingle, Notifications: calendar.NotificationsNone})
	if err != nil {
		t.Fatal(err)
	}
	if provider.updateCalls != 1 || result.Event == nil || result.Event.ID != "google:event" || len(model.upserts) != 1 || model.upserts[0].ID != "event" || len(model.scheduled) != 1 {
		t.Fatalf("provider=%d result=%#v upserts=%#v scheduled=%#v", provider.updateCalls, result, model.upserts, model.scheduled)
	}
}

type stubEventReadModel struct {
	upserts     []calendar.EventV2
	deleted     []calendar.EventRef
	scheduled   []string
	ensured     []storage.SyncWindow
	upsertErr   error
	deleteErr   error
	scheduleErr error
}

func (m *stubEventReadModel) ListCachedEvents(context.Context, []string, time.Time, time.Time) ([]calendar.EventV2, []storage.CachedSourceStatus, error) {
	return nil, nil, nil
}
func (m *stubEventReadModel) EnsureCalendarSyncStates(_ context.Context, _ time.Time, window storage.SyncWindow) error {
	m.ensured = append(m.ensured, window)
	return nil
}
func (m *stubEventReadModel) ScheduleCalendarSync(_ context.Context, calendarID string, _ time.Time, resetCursor bool) error {
	if resetCursor {
		return errors.New("unexpected cursor reset")
	}
	m.scheduled = append(m.scheduled, calendarID)
	return m.scheduleErr
}
func (m *stubEventReadModel) UpsertCachedEvent(_ context.Context, event calendar.EventV2, _ time.Time) error {
	m.upserts = append(m.upserts, event)
	return m.upsertErr
}
func (m *stubEventReadModel) DeleteCachedEvent(_ context.Context, ref calendar.EventRef, _ time.Time) error {
	m.deleted = append(m.deleted, ref)
	return m.deleteErr
}

func TestCloneWithEventReadModelDoesNotChangeSharedService(t *testing.T) {
	provider := &stubV2Provider{name: "google", capabilities: calendar.CalendarCapabilities{
		NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone},
	}, created: &calendar.EventV2{ID: "event", CalendarID: "primary"}}
	shared := New(calendar.NewRegistry([]calendar.Provider{provider}))
	model := &stubEventReadModel{}
	ui := shared.CloneWithEventReadModel(model, storage.SyncWindow{Start: time.Now().UTC(), End: time.Now().UTC().Add(time.Hour)})
	request := calendar.CreateEventRequestV2{CalendarID: "google:primary", Event: calendar.EventCreateV2{Start: calendar.EventTime{Date: "2026-08-22"}, End: calendar.EventTime{Date: "2026-08-23"}}}
	if _, _, err := ui.CreateEventWithReconciliation(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(model.upserts) != 1 || len(model.scheduled) != 1 {
		t.Fatalf("UI clone did not reconcile: %#v", model)
	}
	if _, err := shared.CreateEvent(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(model.upserts) != 1 || len(model.scheduled) != 1 {
		t.Fatalf("shared service leaked UI read model: %#v", model)
	}
}

func TestListEventsFanOutMarksProviderFailureIncomplete(t *testing.T) {
	good := &stubV2Provider{
		name:      "google",
		calendars: []calendar.Calendar{{ID: "primary"}},
		page: calendar.Page[calendar.EventV2]{
			Items:    []calendar.EventV2{{ID: "event"}},
			Complete: true,
		},
	}
	bad := &stubV2Provider{name: "apple", calendarsErr: errors.New("unavailable")}
	service := New(calendar.NewRegistry([]calendar.Provider{good, bad}))

	page, err := service.ListEvents(context.Background(), calendar.ListEventsRequestV2{
		Start: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if page.Complete {
		t.Fatal("page.Complete = true, want false for provider failure")
	}
	if len(page.Items) != 1 || page.Items[0].ID != "google:event" {
		t.Fatalf("items = %#v", page.Items)
	}
	if len(page.Sources) != 2 {
		t.Fatalf("sources = %#v, want two statuses", page.Sources)
	}
}

func TestListEventsFanOutDrainsProviderPagination(t *testing.T) {
	p := &stubV2Provider{name: "google", calendars: []calendar.Calendar{{ID: "primary"}}}
	p.pageFunc = func(request calendar.ListEventsRequestV2) calendar.Page[calendar.EventV2] {
		p.pageTokens = append(p.pageTokens, request.PageToken)
		if request.PageToken == "" {
			return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "second", Start: calendar.EventTime{Date: "2026-08-11"}}}, NextPageToken: "next", Complete: true}
		}
		return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "first", Start: calendar.EventTime{Date: "2026-08-10"}}}, Complete: true}
	}
	service := New(calendar.NewRegistry([]calendar.Provider{p}))

	page, err := service.ListEvents(context.Background(), calendar.ListEventsRequestV2{
		Start: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), PageToken: "must-not-be-forwarded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "google:first" || page.Items[1].ID != "google:second" {
		t.Fatalf("items = %#v", page.Items)
	}
	if len(p.pageTokens) != 2 || p.pageTokens[0] != "" || p.pageTokens[1] != "next" {
		t.Fatalf("page tokens = %#v", p.pageTokens)
	}
}

type stubV2Provider struct {
	name          string
	calendars     []calendar.Calendar
	calendarsErr  error
	capabilities  calendar.CalendarCapabilities
	page          calendar.Page[calendar.EventV2]
	pageFunc      func(calendar.ListEventsRequestV2) calendar.Page[calendar.EventV2]
	pageTokens    []string
	created       *calendar.EventV2
	createCalled  bool
	createCalls   int
	createRequest calendar.CreateEventRequestV2
	updated       *calendar.OperationResult
	updateCalls   int
	deleteCalls   int
}

func (p *stubV2Provider) Name() string { return p.name }
func (p *stubV2Provider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	return p.calendars, p.calendarsErr
}
func (p *stubV2Provider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (p *stubV2Provider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return &calendar.Event{}, nil
}
func (p *stubV2Provider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return &calendar.Event{}, nil
}
func (p *stubV2Provider) DeleteEvent(context.Context, string, string) error { return nil }
func (p *stubV2Provider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return p.capabilities, nil
}

func (p *stubV2Provider) ListEventsV2(_ context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if p.pageFunc != nil {
		return p.pageFunc(request), nil
	}
	return p.page, nil
}
func (p *stubV2Provider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	return &calendar.EventV2{}, nil
}
func (p *stubV2Provider) CreateEventV2(_ context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.createCalled = true
	p.createCalls++
	p.createRequest = request
	if p.created == nil {
		return &calendar.EventV2{}, nil
	}
	copy := *p.created
	return &copy, nil
}

func (p *stubV2Provider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updateCalls++
	if p.updated != nil {
		return p.updated, nil
	}
	return &calendar.OperationResult{Status: "completed"}, nil
}
func (p *stubV2Provider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.deleteCalls++
	return &calendar.OperationResult{Status: "completed"}, nil
}
