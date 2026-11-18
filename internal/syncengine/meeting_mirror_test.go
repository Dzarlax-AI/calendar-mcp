package syncengine

import (
	"calendar-mcp/internal/storage"
	"errors"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestCacheCopyMeetingCreateAndHash(t *testing.T) {
	raw := "https://meet.google.com/abc-defg-hij?authuser=1&token=a%2Bb"
	event := calendar.EventV2{InstanceKind: "cache_instance", MirrorMeetingToGoogle: true, Description: "Source agenda", Conference: &calendar.ConferenceData{EntryPoints: []calendar.ConferenceEntryPoint{{Type: "video", URI: raw}}}}
	created := mirrorCreate("rule", event, "google")
	if !strings.Contains(created.Description, "calendar-mcp:meeting:start") || !strings.Contains(created.Description, "token=a%2Bb") || created.Conference != nil || len(created.Attendees) != 0 {
		t.Fatal("meeting create missing managed link or violates safety")
	}
	if hashEvent(event) == hashEvent(calendar.EventV2{InstanceKind: "cache_instance", MirrorMeetingToGoogle: true, Description: "Source agenda"}) {
		t.Fatal("meeting changes must affect hash")
	}
	event.Description = raw
	created = mirrorCreate("rule", event, "google")
	if strings.Contains(created.Description, "calendar-mcp:meeting:start") {
		t.Fatal("duplicate meeting link")
	}
	event.Conference = nil
	event.SourceConferenceComplete = false
	if patch := mirrorPatch(event); patch.MirrorMeeting == nil || patch.MirrorMeeting.Known {
		t.Fatal("unknown source should preserve marker")
	}
	event.SourceConferenceComplete = true
	if patch := mirrorPatch(event); patch.MirrorMeeting == nil || !patch.MirrorMeeting.Known || patch.MirrorMeeting.URL != "" {
		t.Fatal("confirmed removal not represented")
	}
}

func TestMeetingHashRetainsKnownLinkAcrossUnknown(t *testing.T) {
	event := calendar.EventV2{InstanceKind: "cache_instance", MirrorMeetingToGoogle: true, Title: "first", SourceConferenceComplete: true, Conference: &calendar.ConferenceData{EntryPoints: []calendar.ConferenceEntryPoint{{Type: "video", URI: "https://meet.google.com/abc-defg-hij?authuser=1"}}}}
	known := hashEvent(event)
	event.SourceConferenceComplete = false
	event.Conference = nil
	if got := hashEventForMapping(event, known); got != known {
		t.Fatal("unknown caused a gratuitous write")
	}
	event.Title = "changed"
	changed := hashEventForMapping(event, known)
	if changed == known || !strings.HasSuffix(changed, strings.Split(known, ":meeting:")[1]) {
		t.Fatal("unknown with ordinary edit lost prior link digest")
	}
	if got := hashEventForMapping(event, changed); got != changed {
		t.Fatal("ordinary update not stable")
	}
	event.SourceConferenceComplete = true
	removed := hashEventForMapping(event, changed)
	if removed == changed || strings.Contains(removed, ":meeting:") {
		t.Fatal("confirmed removal not detected")
	}
	if got := hashEventForMapping(event, removed); got != removed {
		t.Fatal("confirmed removal not stable")
	}
	legacy := hashEvent(calendar.EventV2{InstanceKind: "cache_instance"})
	if got := hashEventForMapping(calendar.EventV2{InstanceKind: "cache_instance"}, legacy); got != legacy {
		t.Fatal("legacy mapping changed")
	}
}

func TestCachedBatchMeetingKnownUnknownRemoved(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	event := calendar.EventV2{CalendarID: "google:source", ID: "event", SourceAttendeesComplete: true, SourceConferenceComplete: true, Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, Conference: &calendar.ConferenceData{EntryPoints: []calendar.ConferenceEntryPoint{{Type: "video", URI: "https://meet.google.com/abc-defg-hij?authuser=1"}}}}
	target := &fakeV2Provider{name: "google"}
	mappings := &fakeMappings{values: map[string]storage.Mapping{}}
	engine := testNew(calendar.NewRegistry([]calendar.Provider{target}), mappings)
	rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "google:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	batch := cacheExecutionBatch{Events: []calendar.EventV2{event}, Timezone: "UTC", WindowStart: now, WindowEnd: now.Add(24 * time.Hour), HistoryComplete: true}
	if _, err := engine.runCachedBatch(t.Context(), rule, batch, false); err != nil {
		t.Fatal(err)
	}
	event.Conference = nil
	event.SourceConferenceComplete = false
	batch.Events[0] = event
	if _, err := engine.runCachedBatch(t.Context(), rule, batch, false); err != nil || target.updated != 0 {
		t.Fatalf("unknown caused write: %v", err)
	}
	event.SourceConferenceComplete = true
	batch.Events[0] = event
	if _, err := engine.runCachedBatch(t.Context(), rule, batch, false); err != nil || target.updated != 1 {
		t.Fatalf("removal was missed: %v", err)
	}
	if _, err := engine.runCachedBatch(t.Context(), rule, batch, false); err != nil || target.updated != 1 {
		t.Fatalf("repeat caused write: %v", err)
	}
}

func TestMeetingMarkerAmbiguityFailsClosed(t *testing.T) {
	event := calendar.EventV2{ID: "event", CalendarID: "google:source", Description: "<!-- calendar-mcp:meeting:start -->", DescriptionFormat: "html", Start: calendar.EventTime{Date: "2026-11-18"}, End: calendar.EventTime{Date: "2026-11-19"}}
	_, err := cacheCopyCreate("rule", event.CalendarID, event, "google")
	if !errors.Is(err, calendar.ErrMeetingMarkerAmbiguous) || FailureCode(err) != "sync_meeting_marker_ambiguous" {
		t.Fatalf("missing safe marker diagnostic: %v", err)
	}
}

func TestMeetingMirroringOnlyForGoogleTargets(t *testing.T) {
	event := calendar.EventV2{InstanceKind: "cache_instance", Conference: &calendar.ConferenceData{EntryPoints: []calendar.ConferenceEntryPoint{{Type: "video", URI: "https://meet.google.com/abc-defg-hij"}}}}
	without := event
	without.Conference = nil
	if hashEvent(event) != hashEvent(without) || mirrorPatch(event).MirrorMeeting != nil || strings.Contains(mirrorCreate("rule", event, "apple").Description, "calendar-mcp:meeting:start") {
		t.Fatal("meeting mirroring changed non-Google target")
	}
}
