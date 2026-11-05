package syncengine

import (
	"calendar-mcp/internal/calendar"
	"testing"
)

func TestCacheCopyMaterializeUsesBaseAndPreservesRemovalReason(t *testing.T) {
	day := calendar.EventTime{Date: "2026-11-05"}
	end := calendar.EventTime{Date: "2026-11-06"}
	old := calendar.EventV2{CalendarID: "source", ID: "old", Start: day, End: end}
	kept := calendar.EventV2{CalendarID: "source", ID: "kept", Start: day, End: end}
	added := calendar.EventV2{CalendarID: "source", ID: "new", Start: day, End: end}
	pages := []calendar.EventSyncPage{{SourceTimeZone: "UTC", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "new"}, Event: added}}}, {SourceTimeZone: "UTC", Complete: true, DeletedEventIDs: []string{"old"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "old", Reason: calendar.EventSyncWindowEviction}}}}
	events, removals, err := cacheCopyMaterialize("source", "UTC", calendar.EventSyncIncremental, []calendar.EventV2{old, kept}, true, pages)
	if err != nil || len(events) != 2 || events[0].ID != "kept" || events[1].ID != "new" || len(removals) != 1 || removals[0].Reason != calendar.EventSyncWindowEviction {
		t.Fatalf("invalid result: %d %d %v", len(events), len(removals), err)
	}
}

func TestCacheCopyMaterializeRejectsIncompleteAndAmbiguousChanges(t *testing.T) {
	event := calendar.EventV2{CalendarID: "source", ID: "same", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	good := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "same"}, Event: event}}}
	cases := []struct {
		mode    calendar.EventSyncMode
		base    []calendar.EventV2
		hasBase bool
		pages   []calendar.EventSyncPage
	}{
		{calendar.EventSyncIncremental, nil, false, []calendar.EventSyncPage{good}},
		{calendar.EventSyncReplacement, nil, false, []calendar.EventSyncPage{{SourceTimeZone: "Europe/Belgrade", Complete: true}}},
		{calendar.EventSyncReplacement, []calendar.EventV2{event}, true, []calendar.EventSyncPage{good}},
		{calendar.EventSyncReplacement, nil, false, []calendar.EventSyncPage{{Complete: true, DeletedEventIDs: []string{"x"}}}},
		{calendar.EventSyncReplacement, nil, false, []calendar.EventSyncPage{{Complete: true, Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "same"}, Event: event}, {Object: calendar.SyncObject{ObjectID: "same"}, Event: event}}}}},
		{calendar.EventSyncReplacement, nil, false, []calendar.EventSyncPage{{Complete: true, Warnings: []calendar.EventSyncWarning{{}}}}},
	}
	for i, c := range cases {
		if _, _, err := cacheCopyMaterialize("source", "UTC", c.mode, c.base, c.hasBase, c.pages); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestCacheCopyMaterializeRejectsDuplicateOccurrenceAcrossBaseAndDelta(t *testing.T) {
	day := calendar.EventTime{Date: "2026-11-05"}
	end := calendar.EventTime{Date: "2026-11-06"}
	original := day
	first := calendar.EventV2{CalendarID: "source", ID: "old-id", RecurringEventID: "series", OriginalStart: &original, Start: day, End: end}
	second := first
	second.ID = "new-id"
	page := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: second.ID}, Event: second}}}
	if _, _, err := cacheCopyMaterialize("source", "UTC", calendar.EventSyncIncremental, []calendar.EventV2{first}, true, []calendar.EventSyncPage{page}); err == nil {
		t.Fatal("accepted duplicate occurrence identity")
	}
}
