package syncengine

import (
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func TestMirrorDescriptionEscapesPlainTextAndParticipants(t *testing.T) {
	event := calendar.EventV2{Description: "Agenda <script>\nsecond line", DescriptionFormat: "text", Attendees: []calendar.AttendeeV2{
		{PersonV2: calendar.PersonV2{Name: "<img>", Email: "person@example.invalid"}},
		{PersonV2: calendar.PersonV2{Name: "Name only"}},
		{PersonV2: calendar.PersonV2{Email: "other@example.invalid"}},
		{PersonV2: calendar.PersonV2{}},
	}}
	got := mirrorDescription(event)
	for _, want := range []string{"Agenda &lt;script&gt;<br>second line", "Known participants", "&lt;img&gt; &lt;person@example.invalid&gt;", "Name only", "other@example.invalid"} {
		if !strings.Contains(got, want) {
			t.Fatal("missing expected escaped description content")
		}
	}
	if strings.Contains(got, "<img>") || strings.Contains(got, "<script>") {
		t.Fatal("unescaped plain text or attendee")
	}
	if strings.Count(got, "<br>• ") != 3 {
		t.Fatal("empty attendee was not omitted")
	}
}

func TestMirrorDescriptionIsStableAndPreservesSourceHTML(t *testing.T) {
	a := calendar.AttendeeV2{PersonV2: calendar.PersonV2{Name: "Alice", Email: "a@example.invalid"}}
	b := calendar.AttendeeV2{PersonV2: calendar.PersonV2{Name: "Bob", Email: "b@example.invalid"}}
	event := calendar.EventV2{Description: "<p>Original</p>", DescriptionFormat: "html", Attendees: []calendar.AttendeeV2{b, a, a}}
	first := mirrorDescription(event)
	event.Attendees = []calendar.AttendeeV2{a, b}
	second := mirrorDescription(event)
	if first != second || !strings.HasPrefix(first, "<p>Original</p>") || strings.Count(first, "a@example.invalid") != 1 {
		t.Fatal("ordering, deduplication, or source HTML changed")
	}
	// A mirrored event with no attendees keeps the existing text exactly; it does
	// not append a second copy of the participant section.
	copied := calendar.EventV2{Description: first, DescriptionFormat: "html"}
	if mirrorDescription(copied) != first {
		t.Fatal("participant section accumulated")
	}
}

func TestEngineUpdatesParticipantTextOnlyWhenSourceChanges(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{{ID: "event", Description: "Agenda", DescriptionFormat: "text", Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Name: "Alice", Email: "a@example.invalid"}}}, Start: calendar.EventTime{DateTime: "2026-08-20T13:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T14:00:00Z", TimeZone: "UTC"}}}}
	target := &fakeV2Provider{name: "microsoft"}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context(), rule, false); err != nil || target.updated != 0 {
		t.Fatalf("unchanged source caused update: %v", err)
	}
	source.events[0].Attendees = append(source.events[0].Attendees, calendar.AttendeeV2{PersonV2: calendar.PersonV2{Name: "Bob", Email: "b@example.invalid"}})
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if target.updated != 1 || !target.lastUpdate.RequireNoAttendees || target.lastUpdate.Patch.Attendees.Present || !strings.Contains(target.lastUpdate.Patch.Description.Value, "b@example.invalid") {
		t.Fatal("participant text change was not safely mirrored")
	}
	if _, err := engine.Run(t.Context(), rule, false); err != nil || target.updated != 1 {
		t.Fatalf("repeated source caused duplicate update: %v", err)
	}
}

func TestEnginePreservesParticipantTextAcrossUnknownSourceVisibility(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	event := calendar.EventV2{ID: "event", SourceAttendeesComplete: true, Description: "Original agenda", DescriptionFormat: "text", Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Name: "Alice", Email: "a@example.invalid"}}}, Start: calendar.EventTime{DateTime: "2026-08-20T13:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T14:00:00Z", TimeZone: "UTC"}}
	source := &fakeV2Provider{name: "google", sourceCompletenessExplicit: true, events: []calendar.EventV2{event}}
	target := &fakeV2Provider{name: "microsoft"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	source.events[0].SourceAttendeesComplete = false
	source.events[0].Attendees = nil
	source.events[0].Description = "Changed while attendees hidden"
	source.events[0].Title = "Changed title"
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || result.Warnings == 0 || result.ParticipantsIncomplete != 1 || target.updated != 1 || target.lastUpdate.Patch.Description.Present || target.lastUpdate.Patch.Title.Value != "Changed title" {
		t.Fatalf("unknown source did not preserve description and update title: %v", err)
	}
	source.events[0].Description = "Another hidden agenda"
	source.events[0].Attendees = []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Name: "Bob"}}}
	if _, err := engine.Run(t.Context(), rule, false); err != nil || target.updated != 1 {
		t.Fatalf("changes to incomplete participant context caused a write: %v", err)
	}
	source.events[0].Description = "Changed while attendees hidden"
	source.events[0].SourceAttendeesComplete = true
	source.events[0].Attendees = event.Attendees
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if target.updated != 2 || !target.lastUpdate.Patch.Description.Present || !strings.Contains(target.lastUpdate.Patch.Description.Value, "Changed while attendees hidden") || !strings.Contains(target.lastUpdate.Patch.Description.Value, "a@example.invalid") || target.lastUpdate.Patch.Attendees.Present || !target.lastUpdate.RequireNoAttendees {
		t.Fatal("known source did not safely update participant text")
	}
	if _, err := engine.Run(t.Context(), rule, false); err != nil || target.updated != 2 {
		t.Fatalf("unchanged source caused another write: %v", err)
	}
	source.events[0].Attendees = nil // Explicitly complete empty list is a real removal.
	if _, err := engine.Run(t.Context(), rule, false); err != nil || target.updated != 3 || strings.Contains(target.lastUpdate.Patch.Description.Value, "a@example.invalid") {
		t.Fatalf("known empty list not mirrored: %v", err)
	}
}

func TestEngineAllowsIncompleteSourceWithOtherEvents(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	event := calendar.EventV2{ID: "first", SourceAttendeesComplete: true, Start: calendar.EventTime{Date: "2026-08-20"}, End: calendar.EventTime{Date: "2026-08-21"}}
	unknown := event
	unknown.ID = "second"
	unknown.SourceAttendeesComplete = false
	source := &fakeV2Provider{name: "google", sourceCompletenessExplicit: true, events: []calendar.EventV2{event, unknown}}
	target := &fakeV2Provider{name: "microsoft"}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || target.created != 2 || result.Warnings == 0 || result.ParticipantsIncomplete != 1 {
		t.Fatalf("incomplete source did not create events with warning: %v", err)
	}
}

func TestEngineUnknownDuplicateKeepsConservativeDescription(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	known := calendar.EventV2{ID: "same", SourceAttendeesComplete: true, Start: calendar.EventTime{Date: "2026-08-20"}, End: calendar.EventTime{Date: "2026-08-21"}}
	unknown := known
	unknown.SourceAttendeesComplete = false
	source := &fakeV2Provider{name: "google", sourceCompletenessExplicit: true, events: []calendar.EventV2{known, unknown}}
	target := &fakeV2Provider{name: "microsoft"}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	result, err := engine.Run(t.Context(), rule, false)
	if err != nil || target.created != 1 || result.Warnings == 0 {
		t.Fatalf("unknown duplicate did not propagate warning: %v", err)
	}
}

func TestMirrorDescriptionIncludesExplicitOrganizerOnce(t *testing.T) {
	organizer := calendar.PersonV2{Name: "<Owner>", Email: "owner@example.invalid"}
	event := calendar.EventV2{SourceAttendeesComplete: true, Organizer: &organizer, Attendees: []calendar.AttendeeV2{{PersonV2: organizer}}}
	rendered := mirrorDescription(event)
	if strings.Count(rendered, "owner@example.invalid") != 1 || !strings.Contains(rendered, "&lt;Owner&gt;") {
		t.Fatal("organizer was not escaped or deduplicated")
	}
	event.Attendees = nil
	if mirrorDescription(event) != rendered {
		t.Fatal("duplicate organizer changed output")
	}
	created := mirrorCreate("rule", event, "google")
	patch := mirrorPatch(event)
	if len(created.Attendees) != 0 || patch.Attendees.Present || created.Description != rendered || patch.Description.Value != rendered {
		t.Fatal("organizer was propagated as an invitee or not as text")
	}
	without := event
	without.Organizer = nil
	if hashEvent(without) == hashEvent(event) {
		t.Fatal("organizer text did not affect content hash")
	}
}

func TestRecoveredMarkerWithIncompleteParticipantsPreservesDescription(t *testing.T) {
	original := calendar.EventTime{Date: "2026-10-01"}
	sourceEvent := calendar.EventV2{ID: "occurrence", InstanceKind: calendar.OrphanOccurrence, RecurringEventID: "master", OriginalStart: &original, Start: original, End: calendar.EventTime{Date: "2026-10-02"}, Title: "Updated title", Description: "Source text", SourceAttendeesComplete: false}
	source := &fakeV2Provider{name: "google", sourceCompletenessExplicit: true, events: []calendar.EventV2{sourceEvent}}
	target := &fakeV2Provider{name: "microsoft", recovered: &calendar.EventV2{ID: "existing", Title: "Old title", Description: "Prior participants"}}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if target.created != 0 || target.updated != 1 || target.lastUpdate.Patch.Description.Present || target.lastUpdate.Patch.Title.Value != "Updated title" || target.lastUpdate.Patch.Attendees.Present {
		t.Fatal("recovery did not preserve existing description")
	}
}

func TestIncompleteSourceCreateLabelsPartialParticipants(t *testing.T) {
	start := calendar.EventTime{Date: "2026-11-05"}
	event := calendar.EventV2{ID: "one", SourceAttendeesComplete: false, Title: "Important", Start: start, End: calendar.EventTime{Date: "2026-11-06"}, Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Name: "<Alice>", Email: "alice@example.invalid"}}}}
	created := mirrorCreate("rule", event, "google")
	if len(created.Attendees) != 0 || !strings.Contains(created.Description, "list may be incomplete") || !strings.Contains(created.Description, "&lt;Alice&gt;") {
		t.Fatal("partial attendees were not rendered safely")
	}
}

func TestRestoredOccurrenceWithIncompleteParticipantsKeepsDescription(t *testing.T) {
	start := calendar.EventTime{Date: "2026-10-01"}
	end := calendar.EventTime{Date: "2026-10-02"}
	master := calendar.EventV2{ID: "master", InstanceKind: "seriesMaster", SourceAttendeesComplete: true, Start: start, End: end, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	occurrence := calendar.EventV2{ID: "occurrence", InstanceKind: "occurrence", RecurringEventID: "master", OriginalStart: &start, Start: start, End: end, Title: "Updated", SourceAttendeesComplete: false}
	source := &fakeV2Provider{name: "google", sourceCompletenessExplicit: true, events: []calendar.EventV2{master, occurrence}}
	target := &fakeV2Provider{name: "microsoft"}
	original := eventOriginalStart(occurrence)
	mappings := &fakeMappings{values: map[string]storage.Mapping{
		"master-map":    {ID: "master-map", RuleID: "rule", ObjectKind: "series", SourceEventID: "master", TargetEventID: "target-master", ContentHash: hashEvent(master)},
		"exception-map": {ID: "exception-map", RuleID: "rule", ObjectKind: "exception", SourceEventID: "old-exception", SourceSeriesID: "master", OriginalStart: original, TargetEventID: "target-instance"},
	}}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if target.updated != 1 || target.lastUpdate.Patch.Description.Present || target.lastUpdate.Patch.Title.Value != "Updated" {
		t.Fatal("restoration did not preserve previous description")
	}
	if _, err := engine.Run(t.Context(), rule, false); err != nil || target.updated != 1 {
		t.Fatalf("unchanged incomplete restoration caused a write: %v", err)
	}
	source.events[1].SourceAttendeesComplete = true
	source.events[1].Description = "Restored agenda"
	if _, err := engine.Run(t.Context(), rule, false); err != nil || target.updated != 2 || !target.lastUpdate.Patch.Description.Present || !strings.Contains(target.lastUpdate.Patch.Description.Value, "Restored agenda") {
		t.Fatalf("complete restoration did not update description: %v", err)
	}
	if _, ok := mappings.values["exception-map"]; ok {
		t.Fatal("completed restoration retained exception mapping")
	}
}
