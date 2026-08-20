package syncengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type fakeMappings struct{ values map[string]storage.Mapping }

func (m *fakeMappings) ListMappings(_ context.Context, ruleID string) ([]storage.Mapping, error) {
	var out []storage.Mapping
	for _, v := range m.values {
		if v.RuleID == ruleID {
			out = append(out, v)
		}
	}
	return out, nil
}
func (m *fakeMappings) UpsertMapping(_ context.Context, value storage.Mapping) error {
	m.values[value.ID] = value
	return nil
}
func (m *fakeMappings) DeleteMapping(_ context.Context, id string) error {
	delete(m.values, id)
	return nil
}

type fakeV2Provider struct {
	name                      string
	events                    []calendar.EventV2
	instances                 []calendar.EventV2
	lastRequest               calendar.ListEventsRequestV2
	lastUpdate                calendar.UpdateEventRequestV2
	lastDelete                calendar.DeleteEventRequestV2
	created, updated, deleted int
	recurrenceErr             error
	recovered                 *calendar.EventV2
	lookupRuleID              string
	lookupSourceEventID       string
}

func (p *fakeV2Provider) Name() string                                               { return p.name }
func (p *fakeV2Provider) ListCalendars(context.Context) ([]calendar.Calendar, error) { return nil, nil }
func (p *fakeV2Provider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (p *fakeV2Provider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return nil, nil
}
func (p *fakeV2Provider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return nil, nil
}
func (p *fakeV2Provider) DeleteEvent(context.Context, string, string) error { return nil }
func (p *fakeV2Provider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{}, nil
}
func (p *fakeV2Provider) ListEventsV2(_ context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.lastRequest = request
	return calendar.Page[calendar.EventV2]{Items: p.events, Complete: true}, nil
}
func (p *fakeV2Provider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	return nil, nil
}
func (p *fakeV2Provider) GetEventInstancesV2(_ context.Context, _ calendar.InstancesRequestV2) (calendar.Page[calendar.EventV2], error) {
	return calendar.Page[calendar.EventV2]{Items: p.instances, Complete: true}, nil
}
func (p *fakeV2Provider) ValidateRecurrenceWrite(lines []string, _ calendar.EventTime) error {
	if p.recurrenceErr != nil {
		return p.recurrenceErr
	}
	return calendar.ValidateRecurrence(lines)
}
func (p *fakeV2Provider) FindEventBySyncMarkerV2(_ context.Context, _ string, ruleID, sourceEventID string) (*calendar.EventV2, error) {
	p.lookupRuleID, p.lookupSourceEventID = ruleID, sourceEventID
	return p.recovered, nil
}
func (p *fakeV2Provider) CreateEventV2(_ context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.created++
	return &calendar.EventV2{ID: "target-event", CalendarID: request.CalendarID}, nil
}
func (p *fakeV2Provider) UpdateEventV2(_ context.Context, request calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updated++
	p.lastUpdate = request
	return &calendar.OperationResult{Status: "completed"}, nil
}
func (p *fakeV2Provider) DeleteEventV2(_ context.Context, request calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.deleted++
	p.lastDelete = request
	return &calendar.OperationResult{Status: "completed"}, nil
}

func TestEngineHonorsRuleDepthAndDryRun(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	source := &fakeV2Provider{name: "microsoft", events: []calendar.EventV2{{ID: "source-event", Title: "Meeting", Start: calendar.EventTime{DateTime: "2026-08-20T13:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T14:00:00Z", TimeZone: "UTC"}}}}
	target := &fakeV2Provider{name: "google"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "microsoft:source", TargetCalendarID: "google:target", State: "paused", IntervalSeconds: 600, LookbackDays: 7, LookaheadDays: 30, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(context.Background(), rule, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || target.created != 0 || len(mappings.values) != 0 {
		t.Fatalf("dry result=%#v writes=%d mappings=%d", result, target.created, len(mappings.values))
	}
	if !source.lastRequest.Start.Equal(now.AddDate(0, 0, -7)) || !source.lastRequest.End.Equal(now.AddDate(0, 0, 30)) {
		t.Fatalf("window=%s..%s", source.lastRequest.Start, source.lastRequest.End)
	}
	if source.lastRequest.View != calendar.RecurrenceBoth {
		t.Fatalf("view = %q, want recurrence both", source.lastRequest.View)
	}
}

func TestEngineBlocksLossyRecurrenceBeforeTargetMutation(t *testing.T) {
	start := calendar.EventTime{DateTime: "2026-08-21T09:00:00Z", TimeZone: "UTC"}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{{ID: "series", InstanceKind: "seriesMaster", Start: start, End: calendar.EventTime{DateTime: "2026-08-21T10:00:00Z", TimeZone: "UTC"}, Recurrence: []string{"RRULE:FREQ=MONTHLY;BYSETPOS=5;BYDAY=MO"}}}}
	target := &fakeV2Provider{name: "microsoft", recurrenceErr: errors.New("selector unsupported")}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	_, err := engine.Run(context.Background(), rule, true)
	var compatibilityErr *RecurrenceCompatibilityError
	if !errors.As(err, &compatibilityErr) {
		t.Fatalf("error = %v", err)
	}
	if target.created != 0 || target.updated != 0 || target.deleted != 0 {
		t.Fatalf("target mutated: create=%d update=%d delete=%d", target.created, target.updated, target.deleted)
	}
}

func TestEngineRecoversGoogleTargetByMarkerBeforeCreate(t *testing.T) {
	event := calendar.EventV2{ID: "source-event", Title: "Meeting", Start: calendar.EventTime{DateTime: "2026-08-20T13:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T14:00:00Z", TimeZone: "UTC"}}
	source := &fakeV2Provider{name: "microsoft", events: []calendar.EventV2{event}}
	target := &fakeV2Provider{name: "google", recovered: &calendar.EventV2{ID: "already-created"}}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "microsoft:source", TargetCalendarID: "google:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}

	result, err := engine.Run(context.Background(), rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || target.created != 0 || target.lookupRuleID != "rule" || target.lookupSourceEventID != "source-event" {
		t.Fatalf("result=%#v creates=%d lookup=%q/%q", result, target.created, target.lookupRuleID, target.lookupSourceEventID)
	}
	for _, mapping := range mappings.values {
		if mapping.TargetEventID != "already-created" {
			t.Fatalf("mapping target = %q", mapping.TargetEventID)
		}
	}
}

func TestEngineCreatesUpdatesAndDeletesMirror(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	event := calendar.EventV2{ID: "source-event", Title: "Meeting", Start: calendar.EventTime{DateTime: "2026-08-20T13:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T14:00:00Z", TimeZone: "UTC"}}
	source := &fakeV2Provider{name: "microsoft", events: []calendar.EventV2{event}}
	target := &fakeV2Provider{name: "google"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "microsoft:source", TargetCalendarID: "google:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(context.Background(), rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || target.created != 1 || len(mappings.values) != 1 {
		t.Fatalf("create result=%#v writes=%d mappings=%d", result, target.created, len(mappings.values))
	}
	source.events[0].Title = "Renamed"
	result, err = engine.Run(context.Background(), rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || target.updated != 1 {
		t.Fatalf("update result=%#v writes=%d", result, target.updated)
	}
	source.events[0].Status = "cancelled"
	result, err = engine.Run(context.Background(), rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || target.deleted != 1 || len(mappings.values) != 0 {
		t.Fatalf("delete result=%#v writes=%d mappings=%d", result, target.deleted, len(mappings.values))
	}
}

func TestEngineDoesNotTreatBoundedWindowAbsenceAsDeletion(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	source := &fakeV2Provider{name: "microsoft"}
	target := &fakeV2Provider{name: "google"}
	mapping := storage.Mapping{
		ID: "mapping", RuleID: "rule", ObjectKind: "event", SourceEventID: "outside-window",
		TargetEventID: "target-event", ContentHash: "old", LastSeenAt: now.Add(-time.Hour),
	}
	mappings := &fakeMappings{values: map[string]storage.Mapping{mapping.ID: mapping}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "microsoft:source", TargetCalendarID: "google:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}

	result, err := engine.Run(context.Background(), rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 || result.Warnings != 1 || target.deleted != 0 || len(mappings.values) != 1 {
		t.Fatalf("result=%#v target deletes=%d mappings=%d", result, target.deleted, len(mappings.values))
	}
}

func TestEnginePreservesSeriesExceptionsAndCancellations(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	first := calendar.EventTime{DateTime: "2026-08-21T09:00:00Z", TimeZone: "UTC"}
	second := calendar.EventTime{DateTime: "2026-08-22T09:00:00Z", TimeZone: "UTC"}
	source := &fakeV2Provider{name: "microsoft", events: []calendar.EventV2{
		{ID: "master", InstanceKind: "seriesMaster", Title: "Daily", Start: first, End: calendar.EventTime{DateTime: "2026-08-21T10:00:00Z", TimeZone: "UTC"}, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=5"}},
		{ID: "ordinary", InstanceKind: "occurrence", RecurringEventID: "master", OriginalStart: &first, Title: "Daily", Start: first, End: calendar.EventTime{DateTime: "2026-08-21T10:00:00Z", TimeZone: "UTC"}},
		{ID: "exception", InstanceKind: "exception", RecurringEventID: "master", OriginalStart: &first, Title: "Moved", Start: calendar.EventTime{DateTime: "2026-08-21T11:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-21T12:00:00Z", TimeZone: "UTC"}},
		{ID: "cancelled", InstanceKind: "cancelled", RecurringEventID: "master", OriginalStart: &second, Status: "cancelled"},
	}}
	target := &fakeV2Provider{name: "google", instances: []calendar.EventV2{
		{ID: "target-first", OriginalStart: &first, Start: first},
		{ID: "target-second", OriginalStart: &second, Start: second},
	}}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "microsoft:source", TargetCalendarID: "google:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}

	result, err := engine.Run(context.Background(), rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Updated != 1 || result.Deleted != 1 || result.Skipped != 1 {
		t.Fatalf("result = %#v", result)
	}
	if target.created != 1 || target.updated != 1 || target.deleted != 1 {
		t.Fatalf("writes create=%d update=%d delete=%d", target.created, target.updated, target.deleted)
	}
	if target.lastUpdate.Ref.EventID != "target-first" || target.lastUpdate.Scope != calendar.ScopeSingle {
		t.Fatalf("exception update = %#v", target.lastUpdate)
	}
	if target.lastDelete.Ref.EventID != "target-second" || target.lastDelete.Scope != calendar.ScopeSingle {
		t.Fatalf("cancellation delete = %#v", target.lastDelete)
	}
	if len(mappings.values) != 2 {
		t.Fatalf("mapping count = %d, want master and exception", len(mappings.values))
	}
}

func TestEngineRestoresOccurrenceWhenSourceExceptionDisappears(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	original := calendar.EventTime{DateTime: "2026-08-21T09:00:00Z", TimeZone: "UTC"}
	master := calendar.EventV2{ID: "master", InstanceKind: "seriesMaster", Title: "Daily", Start: original, End: calendar.EventTime{DateTime: "2026-08-21T10:00:00Z", TimeZone: "UTC"}, Recurrence: []string{"RRULE:FREQ=DAILY;COUNT=5"}}
	exception := calendar.EventV2{ID: "exception", InstanceKind: "exception", RecurringEventID: "master", OriginalStart: &original, Title: "Moved", Start: calendar.EventTime{DateTime: "2026-08-21T11:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-21T12:00:00Z", TimeZone: "UTC"}}
	source := &fakeV2Provider{name: "microsoft", events: []calendar.EventV2{master, exception}}
	target := &fakeV2Provider{name: "google", instances: []calendar.EventV2{{ID: "target-first", OriginalStart: &original, Start: original}}}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "microsoft:source", TargetCalendarID: "google:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(context.Background(), rule, false); err != nil {
		t.Fatal(err)
	}

	source.events = []calendar.EventV2{master, {ID: "ordinary", InstanceKind: "occurrence", RecurringEventID: "master", OriginalStart: &original, Title: "Daily", Start: original, End: calendar.EventTime{DateTime: "2026-08-21T10:00:00Z", TimeZone: "UTC"}}}
	result, err := engine.Run(context.Background(), rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || result.Skipped != 2 {
		t.Fatalf("result = %#v", result)
	}
	if len(mappings.values) != 1 {
		t.Fatalf("mapping count = %d, want only series mapping", len(mappings.values))
	}
}
