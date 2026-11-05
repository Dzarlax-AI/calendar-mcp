package calendarprojection

import (
	"encoding/json"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestStagedJSONRoundTripPreservesCanonicalOccurrenceAndSparseCancellation(t *testing.T) {
	const sourceA = "google@account-a:shared-calendar"
	const sourceB = "google@account-b:shared-calendar"
	original := calendar.EventTime{DateTime: "2026-11-05T09:00:00+01:00", TimeZone: "Europe/Belgrade"}
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(48 * time.Hour)
	base := calendar.EventV2{ID: "same-provider-instance", CalendarID: sourceA, Provider: "google", RecurringEventID: "same-provider-series", OriginalStart: &original, Start: calendar.EventTime{DateTime: "2026-11-05T09:00:00+01:00", TimeZone: "Europe/Belgrade"}, End: calendar.EventTime{DateTime: "2026-11-05T10:00:00+01:00", TimeZone: "Europe/Belgrade"}}
	initialID, err := Identity(sourceA, base)
	if err != nil {
		t.Fatal(err)
	}
	other := base
	other.CalendarID = sourceB
	otherID, err := Identity(sourceB, other)
	if err != nil {
		t.Fatal(err)
	}
	if initialID == otherID {
		t.Fatal("distinct Google accounts share an occurrence identity")
	}
	moved := base
	moved.Start = calendar.EventTime{DateTime: "2026-11-05T14:00:00+01:00", TimeZone: "Europe/Belgrade"}
	moved.End = calendar.EventTime{DateTime: "2026-11-05T15:00:00+01:00", TimeZone: "Europe/Belgrade"}
	page := calendar.EventSyncPage{SourceTimeZone: "Europe/Belgrade", Complete: true, Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: moved.ID}, Event: moved}}}
	payload, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var restored calendar.EventSyncPage
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	events, observations, err := Materialize(sourceA, "Europe/Belgrade", start, end, calendar.EventSyncIncremental, []calendar.EventV2{base}, true, []calendar.EventSyncPage{restored})
	if err != nil || len(events) != 1 || len(observations) != 0 {
		t.Fatalf("move materialization: events=%d observations=%d err=%v", len(events), len(observations), err)
	}
	movedID, err := Identity(sourceA, events[0])
	if err != nil {
		t.Fatal(err)
	}
	if movedID != initialID || events[0].Start.DateTime != moved.Start.DateTime || events[0].OriginalStart == nil || *events[0].OriginalStart != original {
		t.Fatal("moved occurrence changed identity or lost its original start")
	}
	// Google may provide only an event ID for a cancellation. It must remain
	// resolved against the exact previously materialized occurrence.
	cancellation := calendar.EventSyncPage{SourceTimeZone: "Europe/Belgrade", Complete: true, DeletedEventIDs: []string{moved.ID}, Removals: []calendar.EventSyncRemoval{{ObjectID: moved.ID, Reason: calendar.EventSyncProviderCancellation}}}
	payload, err = json.Marshal(cancellation)
	if err != nil {
		t.Fatal(err)
	}
	restored = calendar.EventSyncPage{}
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	events, observations, err = Materialize(sourceA, "Europe/Belgrade", start, end, calendar.EventSyncIncremental, events, true, []calendar.EventSyncPage{restored})
	if err != nil || len(events) != 0 || len(observations) != 1 {
		t.Fatalf("cancellation materialization: events=%d observations=%d err=%v", len(events), len(observations), err)
	}
	if observations[0].Unresolved || observations[0].PreviousIdentity != initialID || observations[0].Removal.OriginalStart != nil || observations[0].Removal.SeriesID != "" {
		t.Fatal("sparse cancellation lost its resolved status or previous identity")
	}
	if _, _, err := Materialize(sourceB, "Europe/Belgrade", start, end, calendar.EventSyncIncremental, []calendar.EventV2{base}, true, []calendar.EventSyncPage{restored}); err == nil {
		t.Fatal("accepted a base from another account")
	}
}
