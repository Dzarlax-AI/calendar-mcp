package syncengine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type fakeMappings struct {
	values      map[string]storage.Mapping
	failCurrent bool
}

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
	if m.failCurrent && value.ReconciliationState == "current" {
		return errors.New("mapping commit failed")
	}
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
	lastCreate                calendar.CreateEventRequestV2
	uniqueCreatedIDs          bool
	lastDelete                calendar.DeleteEventRequestV2
	created, updated, deleted int
	recurrenceErr             error
	recovered                 *calendar.EventV2
	lookupRuleID              string
	lookupSourceEventID       string
	lookupCount               int
	lastInstancesRequest      calendar.InstancesRequestV2
	filterInstancesByWindow   bool
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
func (p *fakeV2Provider) GetEventInstancesV2(_ context.Context, request calendar.InstancesRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.lastInstancesRequest = request
	if !p.filterInstancesByWindow {
		return calendar.Page[calendar.EventV2]{Items: p.instances, Complete: true}, nil
	}
	items := make([]calendar.EventV2, 0, len(p.instances))
	for _, item := range p.instances {
		candidate := item.OriginalStart
		if candidate == nil {
			candidate = &item.Start
		}
		instant, err := candidate.Instant()
		if err == nil && !instant.Before(request.Start) && instant.Before(request.End) {
			items = append(items, item)
		}
	}
	return calendar.Page[calendar.EventV2]{Items: items, Complete: true}, nil
}
func (p *fakeV2Provider) ValidateRecurrenceWrite(lines []string, _ calendar.EventTime) error {
	if p.recurrenceErr != nil {
		return p.recurrenceErr
	}
	return calendar.ValidateRecurrence(lines)
}
func (p *fakeV2Provider) FindEventBySyncMarkerV2(_ context.Context, _ string, ruleID, sourceEventID string) (*calendar.EventV2, error) {
	p.lookupCount++
	p.lookupRuleID, p.lookupSourceEventID = ruleID, sourceEventID
	return p.recovered, nil
}

func TestDetachedCopiesUseUniqueStableUIDsAndTopLevelScope(t *testing.T) {
	original := calendar.EventTime{Date: "2026-10-01"}
	first := calendar.EventV2{ID: "one", ICalUID: "shared-series-uid", InstanceKind: calendar.OrphanOccurrence, RecurringEventID: "master", OriginalStart: &original, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	second := first
	second.ID = "two"
	a, b := mirrorCreate("rule", first, "apple"), mirrorCreate("rule", second, "apple")
	if a.ICalUID == b.ICalUID || a.ICalUID == first.ICalUID || a.ICalUID != mirrorCreate("rule", first, "apple").ICalUID || len(a.Recurrence) != 0 {
		t.Fatalf("UIDs = %q, %q", a.ICalUID, b.ICalUID)
	}
	if scopeFor(first) != calendar.ScopeSeries {
		t.Fatal("detached copy must use top-level scope")
	}
}

func TestEngineNormalSeriesDoesNotProbeEveryOccurrence(t *testing.T) {
	start := calendar.EventTime{Date: "2026-10-01"}
	master := calendar.EventV2{ID: "master", InstanceKind: "seriesMaster", Start: start, End: calendar.EventTime{Date: "2026-10-02"}, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{master}}
	for i := 0; i < 100; i++ {
		source.events = append(source.events, calendar.EventV2{ID: fmt.Sprint(i), InstanceKind: "occurrence", RecurringEventID: "master", OriginalStart: &start, Start: start, End: master.End})
	}
	target := &fakeV2Provider{name: "apple"}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, true); err != nil {
		t.Fatal(err)
	}
	if target.lookupCount != 0 {
		t.Fatalf("dry-run marker scans = %d", target.lookupCount)
	}
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if target.lookupCount != 1 {
		t.Fatalf("marker scans = %d, want only master lookup", target.lookupCount)
	}
}

func TestEnginePendingDetachedIntentRecoversAfterMappingFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			original := calendar.EventTime{Date: "2026-10-01"}
			instance := calendar.EventV2{ID: "instance", Title: "Meeting", InstanceKind: calendar.OrphanOccurrence, RecurringEventID: "master", OriginalStart: &original, Start: original, End: calendar.EventTime{Date: "2026-10-02"}}
			source := &fakeV2Provider{name: "google", events: []calendar.EventV2{instance}}
			target := &fakeV2Provider{name: "apple"}
			mappings := &fakeMappings{values: map[string]storage.Mapping{}, failCurrent: true}
			engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
			rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "apple:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
			if _, err := engine.Run(t.Context(), rule, false); err == nil {
				t.Fatal("expected mapping failure")
			}
			if target.created != 1 || len(mappings.values) != 1 {
				t.Fatalf("lost durable intent: writes=%d mappings=%d", target.created, len(mappings.values))
			}
			for _, m := range mappings.values {
				if m.ReconciliationState != "pending_create" {
					t.Fatalf("mapping = %#v", m)
				}
			}
			mappings.failCurrent = false
			target.recovered = &calendar.EventV2{ID: "existing-copy"}
			instance.InstanceKind = "occurrence"
			if cancelled {
				instance.Status = "cancelled"
				instance.InstanceKind = "cancelled"
			}
			master := calendar.EventV2{ID: "master", InstanceKind: "seriesMaster", Start: original, End: instance.End, Recurrence: []string{"RRULE:FREQ=DAILY"}}
			source.events = []calendar.EventV2{master, instance}
			result, err := engine.Run(t.Context(), rule, false)
			if err != nil || result.Created != 0 || target.created != 1 {
				t.Fatalf("recovery = %#v, %v", result, err)
			}
			if cancelled {
				if result.Deleted != 1 || len(mappings.values) != 0 || target.lastDelete.Ref.EventID != "existing-copy" || target.lastDelete.Scope != calendar.ScopeSeries {
					t.Fatalf("cancel recovery = %#v", result)
				}
			} else {
				for _, m := range mappings.values {
					if m.TargetEventID != "existing-copy" || m.ReconciliationState != "current" {
						t.Fatalf("recovered mapping = %#v", m)
					}
				}
				if target.lastUpdate.Scope != calendar.ScopeSeries {
					t.Fatal("recovered standalone update used instance scope")
				}
			}
		})
	}
}
func (p *fakeV2Provider) CreateEventV2(_ context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.created++
	p.lastCreate = request
	id := "target-event"
	if p.uniqueCreatedIDs {
		id = fmt.Sprintf("target-%d", p.created)
	}
	return &calendar.EventV2{ID: id, CalendarID: request.CalendarID}, nil
}

func TestEngineCopiesOrphanAndKeepsExpandedModeAfterRecovery(t *testing.T) {
	original := calendar.EventTime{DateTime: "2026-10-01T08:00:00Z", TimeZone: "UTC"}
	orphan := calendar.EventV2{ID: "instance", InstanceKind: "orphanOccurrence", RecurringEventID: "master", OriginalStart: &original, Title: "Meeting", Start: original, End: calendar.EventTime{DateTime: "2026-10-01T09:00:00Z", TimeZone: "UTC"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{orphan}}
	target := &fakeV2Provider{name: "microsoft", uniqueCreatedIDs: true}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, true)
	if err != nil || result.Created != 1 || result.Warnings != 1 || target.created != 0 || len(mappings.values) != 0 {
		t.Fatalf("dry-run = %#v, %v", result, err)
	}
	result, err = engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 1 || result.Warnings != 1 {
		t.Fatalf("first run = %#v, %v", result, err)
	}
	if len(target.lastCreate.Event.Recurrence) != 0 || target.lastCreate.Notifications != calendar.NotificationsNone || target.lastCreate.Event.Title != "Meeting" || target.lastCreate.Event.SyncMarker.SourceEventID != "instance" {
		t.Fatalf("create = %#v", target.lastCreate)
	}
	for _, m := range mappings.values {
		if m.ObjectKind != "detached_occurrence" || m.SourceSeriesID != "master" {
			t.Fatalf("mapping = %#v", m)
		}
	}
	result, err = engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 0 || result.Skipped != 1 || target.created != 1 {
		t.Fatalf("repeat = %#v, %v", result, err)
	}
	orphan.Title = "Changed"
	source.events = []calendar.EventV2{orphan}
	result, err = engine.Run(t.Context(), rule, false)
	if err != nil || result.Updated != 1 || target.lastUpdate.Ref.EventID != "target-1" || target.lastUpdate.Scope != calendar.ScopeSeries {
		t.Fatalf("update = %#v, %v", result, err)
	}
	orphan.InstanceKind = "occurrence"
	master := calendar.EventV2{ID: "master", InstanceKind: "seriesMaster", Title: "Changed", Start: original, End: orphan.End, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	next := orphan
	next.ID = "next-instance"
	next.Start = calendar.EventTime{DateTime: "2026-10-02T08:00:00Z", TimeZone: "UTC"}
	next.OriginalStart = &next.Start
	source.events = []calendar.EventV2{master, orphan, next}
	result, err = engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 1 || result.Warnings != 1 || target.created != 2 || len(target.lastCreate.Event.Recurrence) != 0 {
		t.Fatalf("recovery = %#v, %v", result, err)
	}
	result, err = engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 0 || target.created != 2 {
		t.Fatalf("recovered repeat = %#v, %v", result, err)
	}
	orphan.Status = "cancelled"
	orphan.InstanceKind = "cancelled"
	source.events = []calendar.EventV2{master, orphan, next}
	result, err = engine.Run(t.Context(), rule, false)
	if err != nil || result.Deleted != 1 || target.lastDelete.Ref.EventID != "target-1" || target.lastDelete.Scope != calendar.ScopeSeries {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
}

func TestEngineRecoveredStandalonePreventsSeriesDuplicate(t *testing.T) {
	original := calendar.EventTime{DateTime: "2026-10-01T08:00:00Z", TimeZone: "UTC"}
	master := calendar.EventV2{ID: "master", InstanceKind: "seriesMaster", Title: "Meeting", Start: original, End: calendar.EventTime{DateTime: "2026-10-01T09:00:00Z", TimeZone: "UTC"}, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	instance := master
	instance.ID = "instance"
	instance.InstanceKind = "occurrence"
	instance.RecurringEventID = "master"
	instance.OriginalStart = &original
	instance.Recurrence = nil
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{master, instance}}
	target := &fakeV2Provider{name: "microsoft", recovered: &calendar.EventV2{ID: "existing-standalone"}}
	id := newMappingID("rule", instance.ID, eventOriginalStart(instance))
	mappings := &fakeMappings{values: map[string]storage.Mapping{id: {ID: id, RuleID: "rule", ObjectKind: "detached_occurrence", SourceEventID: instance.ID, SourceSeriesID: "master", OriginalStart: eventOriginalStart(instance), TargetEventID: "pending:" + id, ReconciliationState: "pending_create"}}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 0 || target.created != 0 || result.Warnings != 1 || len(mappings.values) != 1 {
		t.Fatalf("recovery = %#v, %v", result, err)
	}
	for _, m := range mappings.values {
		if m.ObjectKind != "detached_occurrence" || m.TargetEventID != "existing-standalone" {
			t.Fatalf("mapping = %#v", m)
		}
	}
}

func TestEngineOrphanUsesAlreadyMirroredSeries(t *testing.T) {
	original := calendar.EventTime{DateTime: "2026-10-01T08:00:00Z", TimeZone: "UTC"}
	orphan := calendar.EventV2{ID: "instance", InstanceKind: "orphanOccurrence", RecurringEventID: "master", OriginalStart: &original, Title: "Changed", Start: original, End: calendar.EventTime{DateTime: "2026-10-01T09:00:00Z", TimeZone: "UTC"}}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{orphan}}
	target := &fakeV2Provider{name: "microsoft", instances: []calendar.EventV2{{ID: "target-instance", Start: original, OriginalStart: &original}}}
	mappings := &fakeMappings{values: map[string]storage.Mapping{"series": {ID: "series", RuleID: "rule", ObjectKind: "series", SourceEventID: "master", TargetEventID: "target-master"}}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 0 || result.Updated != 1 || target.created != 0 || target.lastUpdate.Ref.EventID != "target-instance" || target.lastUpdate.Scope != calendar.ScopeSingle {
		t.Fatalf("existing series = %#v, %v", result, err)
	}
}

func TestEngineAllDayOrphanDoesNotBlockOtherEvents(t *testing.T) {
	day := calendar.EventTime{Date: "2026-10-01"}
	orphan := calendar.EventV2{ID: "day", InstanceKind: calendar.OrphanOccurrence, RecurringEventID: "missing", OriginalStart: &day, Title: "All day", Start: day, End: calendar.EventTime{Date: "2026-10-02"}}
	ordinary := calendar.EventV2{ID: "ordinary", Title: "Other", Start: day, End: orphan.End}
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{orphan, ordinary}}
	target := &fakeV2Provider{name: "microsoft", uniqueCreatedIDs: true}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := New(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || result.Created != 2 || result.Warnings != 1 || len(mappings.values) != 2 {
		t.Fatalf("run = %#v, %v", result, err)
	}
	orphan.Status = "cancelled"
	orphan.InstanceKind = "cancelled"
	source.events = []calendar.EventV2{orphan, ordinary}
	result, err = engine.Run(t.Context(), rule, false)
	if err != nil || result.Deleted != 1 || result.Skipped != 1 || len(mappings.values) != 1 || target.lastDelete.Ref.EventID != "target-1" {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
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

func TestHashEventIncludesMirroredVisibilityAndTransparency(t *testing.T) {
	base := calendar.EventV2{ID: "event", Title: "Meeting", Visibility: "private", Transparency: "opaque"}
	visibilityChanged := base
	visibilityChanged.Visibility = "public"
	transparencyChanged := base
	transparencyChanged.Transparency = "transparent"
	if hashEvent(base) == hashEvent(visibilityChanged) {
		t.Fatal("visibility change did not affect content hash")
	}
	if hashEvent(base) == hashEvent(transparencyChanged) {
		t.Fatal("transparency change did not affect content hash")
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
	if result.Created != 0 || target.created != 0 || target.lookupRuleID != "rule" || target.lookupSourceEventID != "source-event" {
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

func TestFindTargetInstanceIncludesOriginalStartOutsideSyncWindow(t *testing.T) {
	tests := []struct {
		name     string
		original calendar.EventTime
	}{
		{name: "timed", original: calendar.EventTime{DateTime: "2026-08-20T09:00:00Z", TimeZone: "UTC"}},
		{name: "all day", original: calendar.EventTime{Date: "2026-08-20"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &fakeV2Provider{
				name:                    "google",
				instances:               []calendar.EventV2{{ID: "target-instance", OriginalStart: &tt.original, Start: tt.original}},
				filterInstancesByWindow: true,
			}
			windowStart := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
			windowEnd := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

			id, found, err := findTargetInstance(context.Background(), provider, "target", "series", &tt.original, windowStart, windowEnd)
			if err != nil {
				t.Fatal(err)
			}
			if !found || id != "target-instance" {
				t.Fatalf("found=%v id=%q", found, id)
			}
			originalInstant, err := tt.original.Instant()
			if err != nil {
				t.Fatal(err)
			}
			if provider.lastInstancesRequest.Start.After(originalInstant) || !provider.lastInstancesRequest.End.After(originalInstant) {
				t.Fatalf("lookup window %s..%s does not include %s", provider.lastInstancesRequest.Start, provider.lastInstancesRequest.End, originalInstant)
			}
		})
	}
}
