package eventsync

import (
	"calendar-mcp/internal/calendar"
	"testing"
)

func TestValidateRemovalProvenanceRejectsInconsistentMetadata(t *testing.T) {
	page := calendar.EventSyncPage{DeletedEventIDs: []string{"id"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "id", Reason: calendar.EventSyncProviderCancellation}}}
	if err := validateRemovalProvenance(page); err != nil {
		t.Fatal(err)
	}
	page.Removals = nil
	if validateRemovalProvenance(page) == nil {
		t.Fatal("missing provenance accepted")
	}
	page.Removals = []calendar.EventSyncRemoval{{ObjectID: "id", Reason: calendar.EventSyncProviderCancellation}}
	page.DeletedEventIDs = append(page.DeletedEventIDs, "unknown")
	if validateRemovalProvenance(page) == nil {
		t.Fatal("partially missing provenance accepted")
	}
	page.DeletedEventIDs = page.DeletedEventIDs[:1]
	page.Removals[0].ObjectID = "other"
	if validateRemovalProvenance(page) == nil {
		t.Fatal("unmatched removal accepted")
	}
	page.Removals[0].ObjectID = "id"
	page.Removals = append(page.Removals, page.Removals[0])
	if validateRemovalProvenance(page) == nil {
		t.Fatal("duplicate removal accepted")
	}
	page.Removals = page.Removals[:1]
	page.Removals[0].Reason = "unknown"
	if validateRemovalProvenance(page) == nil {
		t.Fatal("unknown reason accepted")
	}
}

func TestRunOneStagingIsOptInAndChainsPages(t *testing.T) {
	pages := []calendar.EventSyncPage{
		{DeletedEventIDs: []string{"id"}, Removals: []calendar.EventSyncRemoval{{ObjectID: "id", Reason: calendar.EventSyncProviderCancellation}}, NextPageToken: "next"},
		{Complete: true, NextCursor: "cursor"},
	}
	for _, capture := range []bool{false, true} {
		provider := &fakeProvider{pages: pages}
		store := &fakeStore{}
		service := newService(store, provider)
		service.CaptureRemovalProvenance = capture
		if err := service.RunOne(t.Context(), testState("cursor")); err != nil {
			t.Fatalf("capture %v: %v", capture, err)
		}
		if len(store.applied) != 2 {
			t.Fatalf("capture %v: pages %d", capture, len(store.applied))
		}
		a, b := store.applied[0].batch.RemovalStage, store.applied[1].batch.RemovalStage
		if !capture {
			if a != nil || b != nil {
				t.Fatal("unexpected staging by default")
			}
			continue
		}
		if a == nil || b == nil || a.Ordinal != 0 || b.Ordinal != 1 || a.NextTokenHash != b.PreviousTokenHash || len(a.Removals) != 1 {
			t.Fatal("staging did not preserve chained pages")
		}
	}
}

func TestRunOneCaptureRejectsDeletionWithoutProvenance(t *testing.T) {
	provider := &fakeProvider{pages: []calendar.EventSyncPage{{DeletedEventIDs: []string{"unknown"}, Complete: true, NextCursor: "cursor"}}}
	store := &fakeStore{}
	service := newService(store, provider)
	service.CaptureRemovalProvenance = true
	if err := service.RunOne(t.Context(), testState("cursor")); err != ErrInvalidPage {
		t.Fatalf("error=%v", err)
	}
	if len(store.applied) != 0 {
		t.Fatal("page with unknown deletion was applied")
	}
}
