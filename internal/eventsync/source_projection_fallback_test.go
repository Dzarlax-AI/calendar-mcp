package eventsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type failingProjectionStore struct {
	*fakeStore
	failure  string
	deferred bool
}

func (s *failingProjectionStore) PlanSourceProjectionCapture(context.Context, storage.CalendarSyncState) (storage.SourceProjectionCapturePlan, error) {
	return storage.SourceProjectionCapturePlan{Enabled: true}, nil
}
func (s *failingProjectionStore) StartProjectionPageAttempt(context.Context, storage.CalendarSyncState, storage.ProjectionPageAttemptInput, time.Time) error {
	if s.failure == "start" {
		return errors.New("replica unavailable")
	}
	if s.failure == "start-conflict" {
		return storage.ErrExclusiveDispatchConflict
	}
	return nil
}
func (s *failingProjectionStore) ApplyProjectionStagedPage(ctx context.Context, state storage.CalendarSyncState, _ string, _ int, _, _ string, _ calendar.EventSyncPage, now time.Time) error {
	if s.failure == "page" {
		return errors.New("replica unavailable")
	}
	return s.fakeStore.ApplyEventSyncPage(ctx, state, storage.EventSyncBatch{}, false, now)
}
func (s *failingProjectionStore) FinalizeSourceProjectionPage(context.Context, storage.CalendarSyncState, string, int, string, string, calendar.EventSyncPage, time.Time, time.Time) (int64, error) {
	if s.failure == "final-conflict" {
		return 0, storage.ErrExclusiveDispatchConflict
	}
	return 0, errors.New("replica unavailable")
}

func (s *failingProjectionStore) DeferCalendarSyncForReservation(context.Context, storage.CalendarSyncState, time.Time, time.Time) error {
	s.deferred = true
	return nil
}

func TestReplicaFailureDoesNotStopCalendarSync(t *testing.T) {
	for _, failure := range []string{"start", "page", "final"} {
		t.Run(failure, func(t *testing.T) {
			s := &failingProjectionStore{fakeStore: &fakeStore{}, failure: failure}
			p := &fakeProvider{pages: []calendar.EventSyncPage{{NextPageToken: "next"}, {Complete: true, NextCursor: "next-cursor"}}}
			svc := NewService(s, fakeResolver{provider: p, rawID: "calendar"}, nil)
			if err := svc.RunOne(t.Context(), testState("cursor")); err != nil {
				t.Fatalf("sync failed: %v", err)
			}
			if len(s.applied) != 2 || !s.applied[1].final || s.applied[1].batch.NextCursor != "next-cursor" || len(s.fails) != 0 {
				t.Fatal("legacy cache did not finish")
			}
		})
	}
}

func TestReplicaReservationConflictDefersWithoutAdvancingCursor(t *testing.T) {
	for _, failure := range []string{"start-conflict", "final-conflict"} {
		t.Run(failure, func(t *testing.T) {
			s := &failingProjectionStore{fakeStore: &fakeStore{}, failure: failure}
			p := &fakeProvider{pages: []calendar.EventSyncPage{{NextPageToken: "next"}, {Complete: true, NextCursor: "next-cursor"}}}
			if err := NewService(s, fakeResolver{provider: p, rawID: "calendar"}, nil).RunOne(t.Context(), testState("cursor")); err != nil {
				t.Fatal(err)
			}
			if !s.deferred || len(s.fails) != 0 {
				t.Fatal("reservation conflict did not defer the claim")
			}
			for _, page := range s.applied {
				if page.final {
					t.Fatal("cursor advanced while a copy run was active")
				}
			}
		})
	}
}
