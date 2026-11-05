package storage

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestProjectionPrepareMultiPageAndInvalidation(t *testing.T) {
	for _, scenario := range []string{"valid", "quarantine", "epoch", "lease"} {
		t.Run(scenario, func(t *testing.T) {
			s, now := newEventCacheStore(t)
			ensureProjectionPageStagingSchema(t, s)
			ctx := t.Context()
			if _, err := s.db.ExecContext(ctx, `UPDATE calendars SET timezone='UTC',enabled=1,can_read=1 WHERE id='calendar'`); err != nil {
				t.Fatal(err)
			}
			state, err := s.ClaimDueCalendarSync(ctx, "multi-page-worker", now, now.Add(time.Hour))
			if err != nil || state == nil {
				t.Fatalf("claim: %v", err)
			}
			attempt := ProjectionPageAttemptInput{CalendarID: "calendar", AttemptID: "multi-page-attempt-012345", Timezone: "UTC", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
			if err := s.StartProjectionPageAttempt(ctx, *state, attempt, now); err != nil {
				t.Fatal(err)
			}
			event := func(id string, complete bool) calendar.EventSyncUpsert {
				e := calendar.EventV2{ID: id, CalendarID: "calendar", Provider: "google", SourceAttendeesComplete: complete, Start: calendar.EventTime{DateTime: now.Add(-time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
				return calendar.EventSyncUpsert{Object: calendar.SyncObject{ObjectID: id}, Event: e}
			}
			first := calendar.EventSyncPage{SourceTimeZone: "UTC", NextPageToken: "next", Upserts: []calendar.EventSyncUpsert{event("one", true)}}
			last := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "cursor", Upserts: []calendar.EventSyncUpsert{event("two", false)}}
			if err := s.AppendProjectionPage(ctx, *state, attempt.AttemptID, 0, "", "UTC", first, now); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendProjectionPage(ctx, *state, attempt.AttemptID, 1, "next", "UTC", last, now); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "quarantine":
				_, err = s.db.ExecContext(ctx, `INSERT INTO calendar_sync_quarantine(calendar_id,object_id,error_code,first_seen_at,last_seen_at,next_repair_at,active) VALUES(?,?,?,?,?,?,?)`, "calendar", "broken", "protocol", now, now, now, true)
			case "epoch":
				err = s.beginProjectionMutation(ctx, "calendar", "mutation-test-0123456789", now)
				if err == nil {
					err = s.resolveProjectionMutation(ctx, "calendar", "mutation-test-0123456789")
				}
			case "lease":
				_, err = s.db.ExecContext(ctx, `UPDATE calendar_sync_state SET lease_owner=? WHERE calendar_id=?`, "new-owner", "calendar")
			}
			if err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			got, err := s.prepareProjectionFinalizationTx(ctx, tx, *state, attempt.AttemptID, now)
			switch scenario {
			case "valid":
				if err != nil || len(got.pages) != 2 || len(got.events) != 2 || got.cursor != "cursor" {
					t.Fatalf("candidate: %+v, %v", got, err)
				}
				if !got.events[0].SourceAttendeesComplete || got.events[1].SourceAttendeesComplete {
					t.Fatal("attendee completeness evidence changed")
				}
			case "lease":
				if !errors.Is(err, ErrCalendarSyncLeaseLost) {
					t.Fatalf("lease: %v", err)
				}
			case "epoch":
				if !errors.Is(err, ErrProjectionSnapshotStale) {
					t.Fatalf("epoch: %v", err)
				}
			case "quarantine":
				if !errors.Is(err, ErrProjectionPageStage) {
					t.Fatalf("quarantine: %v", err)
				}
			}
		})
	}
}

func TestProjectionChainRejectsForgedStructuralPages(t *testing.T) {
	const source, zone, cursor = "source", "UTC", "cursor"
	empty, token := projectionPageHash(nil), projectionPageHash([]byte("token"))
	record := func(t *testing.T, ordinal int, page calendar.EventSyncPage, previous, next, terminal string) projectionStagedPage {
		t.Helper()
		payload, err := encodeProjectionPagePayload(page, source)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(struct {
			Payload                json.RawMessage
			Previous, Next, Cursor string
		}{payload, previous, next, terminal})
		if err != nil {
			t.Fatal(err)
		}
		return projectionStagedPage{ordinal: ordinal, digest: projectionPageHash(raw), previous: previous, next: next, payload: payload}
	}
	cases := []struct {
		name                string
		first, last         calendar.EventSyncPage
		firstNext, lastPrev string
	}{
		{"early complete", calendar.EventSyncPage{SourceTimeZone: zone, Complete: true}, calendar.EventSyncPage{SourceTimeZone: zone, Complete: true}, token, token},
		{"missing complete", calendar.EventSyncPage{SourceTimeZone: zone}, calendar.EventSyncPage{SourceTimeZone: zone}, token, token},
		{"wrong timezone", calendar.EventSyncPage{SourceTimeZone: "Europe/Belgrade"}, calendar.EventSyncPage{SourceTimeZone: zone, Complete: true}, token, token},
		{"repeated token", calendar.EventSyncPage{SourceTimeZone: zone}, calendar.EventSyncPage{SourceTimeZone: zone, Complete: true}, empty, empty},
		{"missing removal provenance", calendar.EventSyncPage{SourceTimeZone: zone, DeletedEventIDs: []string{"event"}}, calendar.EventSyncPage{SourceTimeZone: zone, Complete: true}, token, token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := record(t, 0, tc.first, empty, tc.firstNext, "")
			b := record(t, 1, tc.last, tc.lastPrev, empty, cursor)
			_, err := validateProjectionPageChain(source, zone, cursor, empty, int64(len(a.payload)+len(b.payload)), []projectionStagedPage{a, b})
			if !errors.Is(err, ErrProjectionPageStage) {
				t.Fatalf("forged chain accepted: %v", err)
			}
		})
	}
}
