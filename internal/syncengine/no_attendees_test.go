package syncengine

import (
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type undeclaredSafetyProvider struct{ calendar.EventProviderV2 }

func TestEngineRejectsTargetWithoutAttendeeSafetyBeforeWrite(t *testing.T) {
	source := &fakeV2Provider{name: "google"}
	realTarget := &fakeV2Provider{name: "microsoft"}
	target := undeclaredSafetyProvider{EventProviderV2: realTarget}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); !errors.Is(err, ErrUnsafeTargetAttendees) {
		t.Fatalf("unsupported provider: %v", err)
	}
	if realTarget.created+realTarget.updated+realTarget.deleted != 0 {
		t.Fatal("unsupported provider was mutated")
	}
}

func TestSyncTargetRequiresNoAttendeesForEveryWrite(t *testing.T) {
	source := &fakeV2Provider{name: "google"}
	target := &fakeV2Provider{name: "microsoft"}
	registry := calendar.NewRegistry([]calendar.Provider{source, target})
	g := selectionGuardedTarget{EventProviderV2: target, registry: registry, sourceID: "google:source", targetID: "microsoft:target", dispatchFence: testDispatchFence{}}
	invite := calendar.AttendeeV2{Email: "synthetic@example.invalid"}
	_, err := g.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{Event: calendar.EventCreateV2{Attendees: []calendar.AttendeeV2{invite}}})
	if !errors.Is(err, ErrUnsafeTargetAttendees) || target.created != 0 {
		t.Fatalf("attendee payload accepted: %v", err)
	}
	_, err = g.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Patch: calendar.EventPatchV2{Attendees: calendar.PatchField[[]calendar.AttendeeV2]{Present: true, Value: []calendar.AttendeeV2{invite}}}})
	if !errors.Is(err, ErrUnsafeTargetAttendees) || target.updated != 0 {
		t.Fatalf("attendee patch accepted: %v", err)
	}
	_, err = g.CreateEventV2(t.Context(), calendar.CreateEventRequestV2{Notifications: calendar.NotificationsAll})
	if err != nil || !target.lastCreate.RequireNoAttendees || target.lastCreate.Notifications != calendar.NotificationsNone || len(target.lastCreate.Event.Attendees) != 0 {
		t.Fatalf("unsafe create: %v", err)
	}
	_, err = g.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Notifications: calendar.NotificationsAll})
	if err != nil || !target.lastUpdate.RequireNoAttendees || target.lastUpdate.Notifications != calendar.NotificationsNone {
		t.Fatalf("unsafe update: %v", err)
	}
	_, err = g.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Notifications: calendar.NotificationsAll})
	if err != nil || !target.lastDelete.RequireNoAttendees || target.lastDelete.Notifications != calendar.NotificationsNone {
		t.Fatalf("unsafe delete: %v", err)
	}
}

func TestEngineDoesNotCopySourceAttendees(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	source := &fakeV2Provider{name: "google", events: []calendar.EventV2{{ID: "event", Attendees: []calendar.AttendeeV2{{Email: "synthetic@example.invalid"}}, Start: calendar.EventTime{DateTime: "2026-08-20T13:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T14:00:00Z", TimeZone: "UTC"}}}}
	target := &fakeV2Provider{name: "microsoft"}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{source, target}), &fakeMappings{values: map[string]storage.Mapping{}})
	engine.now = func() time.Time { return now }
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "microsoft:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if target.created != 1 || len(target.lastCreate.Event.Attendees) != 0 || !target.lastCreate.RequireNoAttendees || target.lastCreate.Notifications != calendar.NotificationsNone {
		t.Fatal("source attendees were copied or safety flag missing")
	}
}
