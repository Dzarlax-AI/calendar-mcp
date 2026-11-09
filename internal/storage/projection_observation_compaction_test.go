package storage

import (
	"reflect"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
)

func TestProjectionCompactionRetainsDistinctEvidenceAndOrder(t *testing.T) {
	base := calendarprojection.RemovalObservation{Removal: calendar.EventSyncRemoval{ObjectID: "same", Reason: calendar.EventSyncProviderCancellation}, Unresolved: true}
	resolved := base
	resolved.Unresolved = false
	resolved.PreviousIdentity = "source\x00occurrence"
	otherStart := resolved
	otherStart.Removal.OriginalStart = &calendar.EventTime{DateTime: "2026-11-09T10:00:00Z", TimeZone: "UTC"}
	eviction := base
	eviction.Removal.Reason = calendar.EventSyncWindowEviction
	series := base
	series.Removal.SeriesID = "series"
	want := []calendarprojection.RemovalObservation{base, resolved, otherStart, eviction, series}
	input := []calendarprojection.RemovalObservation{base, resolved, base, otherStart, resolved, eviction, series, base}
	got, err := uniqueProjectionObservations(input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("distinct evidence changed: got=%+v err=%v", got, err)
	}
	if !observationSetContains(got, input) {
		t.Fatal("identical prior observations not retained")
	}
	for i := range want {
		missing := append([]calendarprojection.RemovalObservation{}, want[:i]...)
		missing = append(missing, want[i+1:]...)
		if observationSetContains(missing, input) {
			t.Fatalf("missing distinct evidence %d accepted", i)
		}
	}
}
