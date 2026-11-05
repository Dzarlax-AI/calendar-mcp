package calendarprojection

import (
	"calendar-mcp/internal/calendar"
	"testing"
	"time"
)

func TestMaterializeResolvesKnownCancellationWithoutDeletingTargets(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	event := calendar.EventV2{CalendarID: "source", ID: "old", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, DeletedEventIDs: []string{"old", "unknown"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "old", Reason: calendar.EventSyncProviderCancellation}, {ObjectID: "unknown", Reason: calendar.EventSyncProviderCancellation}}}
	events, observations, err := Materialize("source", "UTC", start, start.AddDate(0, 0, 1), calendar.EventSyncIncremental, []calendar.EventV2{event}, true, []calendar.EventSyncPage{page})
	if err != nil || len(events) != 0 || len(observations) != 2 || observations[0].Unresolved || !observations[1].Unresolved || observations[0].PreviousIdentity == "" || observations[1].PreviousIdentity != "" {
		t.Fatalf("invalid observations: events=%d observations=%d err=%v", len(events), len(observations), err)
	}
}

func TestMaterializeRejectsOutsideWindowAndSkippedCivilDates(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	outside := calendar.EventV2{CalendarID: "source", ID: "outside", Start: calendar.EventTime{Date: "2026-11-06"}, End: calendar.EventTime{Date: "2026-11-07"}}
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "outside"}, Event: outside}}}
	if _, _, err := Materialize("source", "UTC", start, start.AddDate(0, 0, 1), calendar.EventSyncReplacement, nil, false, []calendar.EventSyncPage{page}); err == nil {
		t.Fatal("accepted event outside frozen window")
	}
	start = time.Date(2011, 12, 28, 0, 0, 0, 0, time.UTC)
	skipped := calendar.EventV2{CalendarID: "source", ID: "skip", Start: calendar.EventTime{Date: "2011-12-30"}, End: calendar.EventTime{Date: "2011-12-31"}}
	page = calendar.EventSyncPage{SourceTimeZone: "Pacific/Apia", Complete: true, Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "skip"}, Event: skipped}}}
	if _, _, err := Materialize("source", "Pacific/Apia", start, start.AddDate(0, 0, 6), calendar.EventSyncReplacement, nil, false, []calendar.EventSyncPage{page}); err == nil {
		t.Fatal("accepted skipped civil date")
	}
}

func TestMaterializeRejectsConflictingPagesAndWrongTimezone(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	e := calendar.EventV2{CalendarID: "source", ID: "same", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	pages := []calendar.EventSyncPage{{SourceTimeZone: "UTC", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: e.ID}, Event: e}}}, {SourceTimeZone: "UTC", Complete: true, DeletedEventIDs: []string{"same"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "same", Reason: calendar.EventSyncProviderCancellation}}}}
	if _, _, err := Materialize("source", "UTC", start, start.AddDate(0, 0, 1), calendar.EventSyncReplacement, nil, false, pages); err == nil {
		t.Fatal("accepted conflicting pages")
	}
	pages = pages[:1]
	pages[0].Complete = true
	pages[0].SourceTimeZone = "Europe/Belgrade"
	if _, _, err := Materialize("source", "UTC", start, start.AddDate(0, 0, 1), calendar.EventSyncReplacement, nil, false, pages); err == nil {
		t.Fatal("accepted changed timezone")
	}
}

func TestMaterializeEvictionWithoutPriorIdentityRemainsUnresolved(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, DeletedEventIDs: []string{"missing"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "missing", Reason: calendar.EventSyncWindowEviction}}}
	_, observations, err := Materialize("source", "UTC", start, start.AddDate(0, 0, 1), calendar.EventSyncReplacement, nil, false, []calendar.EventSyncPage{page})
	if err != nil || len(observations) != 1 || !observations[0].Unresolved || observations[0].PreviousIdentity != "" {
		t.Fatalf("invalid unresolved eviction: %v", err)
	}
}

func TestCancellationIdentityEvidenceRejectsConflictsAndMasters(t *testing.T) {
	start := calendar.EventTime{DateTime: "2026-11-05T09:00:00Z", TimeZone: "UTC"}
	event := calendar.EventV2{ID: "instance", CalendarID: "source", RecurringEventID: "series", OriginalStart: &start, Start: start, End: calendar.EventTime{DateTime: "2026-11-05T10:00:00Z", TimeZone: "UTC"}}
	identity, err := Identity("source", event)
	if err != nil {
		t.Fatal(err)
	}
	good := calendar.EventSyncRemoval{ObjectID: event.ID, Reason: calendar.EventSyncProviderCancellation}
	if !matchesCancelledInstance("source", event, good, identity) {
		t.Fatal("known sparse cancellation not matched")
	}
	differentStart := calendar.EventTime{DateTime: "2026-11-06T09:00:00Z", TimeZone: "UTC"}
	bad := good
	bad.OriginalStart = &differentStart
	if matchesCancelledInstance("source", event, bad, identity) {
		t.Fatal("mismatched original start accepted")
	}
	bad = good
	bad.SeriesID = "different"
	if matchesCancelledInstance("source", event, bad, identity) {
		t.Fatal("mismatched series accepted")
	}
	bad = good
	bad.RecurrencePresent = true
	if matchesCancelledInstance("source", event, bad, identity) {
		t.Fatal("master removal accepted")
	}
	master := event
	master.InstanceKind = "seriesMaster"
	if matchesCancelledInstance("source", master, good, identity) {
		t.Fatal("master base accepted")
	}
}
