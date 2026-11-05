package storage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func ensureProjectionPageStagingSchema(t *testing.T, s *Store) {
	t.Helper()
	var count int
	if s.dialect == DialectPostgres {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='projection_page_attempts'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projection_page_attempts'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
	}
	if count > 0 {
		return
	}
	path := "migrations/sqlite/014_projection_page_staging.sql"
	if s.dialect == DialectPostgres {
		path = "migrations/postgres/014_projection_page_staging.sql"
	}
	script, err := migrations.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(string(script)); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionPageStagingSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runProjectionPageStagingContract(t, s, "calendar", now)
}

func runProjectionPageStagingContract(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "Europe/Belgrade", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "stage-worker", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim did not return fixture calendar: %+v %v", state, err)
	}
	in := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "page-attempt-0123456789", Timezone: "Europe/Belgrade", Generation: state.Generation, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncReplacement}
	invalid := in
	invalid.Mode = calendar.EventSyncIncremental
	invalid.BaseCursor = "synthetic-cursor"
	if err := s.StartProjectionPageAttempt(ctx, *state, invalid, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("missing base: %v", err)
	}
	if err := s.StartProjectionPageAttempt(ctx, *state, in, now); err != nil {
		t.Fatal(err)
	}
	found, err := s.GetProjectionPageAttemptByID(ctx, id, in.AttemptID)
	if err != nil || found.Input.Generation != in.Generation || found.Input.Mode != in.Mode || found.Input.Timezone != in.Timezone || found.Input.BaseCursor != in.BaseCursor || found.NextOrdinal != 0 || found.Complete {
		t.Fatalf("lookup attempt: %v", err)
	}
	if err := s.StartProjectionPageAttempt(ctx, *state, in, now); err == nil {
		t.Fatal("repeat start created another attempt")
	}
	if _, err := s.GetProjectionPageAttemptByID(ctx, id, "missing-attempt-0123456"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lookup: %v", err)
	}
	first := calendar.EventSyncPage{SourceTimeZone: "Europe/Belgrade", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "one"}, Event: calendar.EventV2{ID: "one", CalendarID: id, SourceAttendeesComplete: true}}}, NextPageToken: "synthetic-secret-page-token"}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, "", "Europe/Belgrade", first, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("out-of-order: %v", err)
	}
	untrustedTZ := first
	untrustedTZ.SourceTimeZone = "UTC"
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "Europe/Belgrade", untrustedTZ, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("mismatched response timezone: %v", err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "", first, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("missing response timezone: %v", err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "UTC", first, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("changed response timezone: %v", err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "Europe/Belgrade", first, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "Europe/Belgrade", first, now); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	var stored []byte
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT payload FROM projection_page_attempt_pages WHERE calendar_id=? AND generation=? AND page_ordinal=?`), id, state.Generation, 0).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	restored, err := decodeProjectionPagePayload(stored, id)
	if err != nil || len(restored.Upserts) != 1 || !restored.Upserts[0].Event.SourceAttendeesComplete {
		t.Fatalf("staged evidence round trip: %v", err)
	}

	mutated := first
	mutated.Upserts = []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "other"}, Event: calendar.EventV2{ID: "other", CalendarID: id}}}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "Europe/Belgrade", mutated, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("changed replay: %v", err)
	}
	repeated := calendar.EventSyncPage{SourceTimeZone: "Europe/Belgrade", NextPageToken: first.NextPageToken}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, string(first.NextPageToken), "Europe/Belgrade", repeated, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("repeated page token: %v", err)
	}
	missing := calendar.EventSyncPage{SourceTimeZone: "Europe/Belgrade", Complete: true, NextCursor: "synthetic-final-cursor", DeletedEventIDs: []string{"gone"}}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, string(first.NextPageToken), "Europe/Belgrade", missing, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("missing provenance: %v", err)
	}
	final := missing
	final.Removals = []calendar.EventSyncRemoval{{ObjectID: "gone", Reason: calendar.EventSyncProviderCancellation}}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, "wrong-token", "Europe/Belgrade", final, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("wrong chain: %v", err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, string(first.NextPageToken), "Europe/Belgrade", final, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, string(first.NextPageToken), "Europe/Belgrade", final, now); err != nil {
		t.Fatalf("final replay: %v", err)
	}
	var payload []byte
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT payload FROM projection_page_attempt_pages WHERE calendar_id=? AND generation=? AND page_ordinal=0`), id, state.Generation).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), string(first.NextPageToken)) {
		t.Fatal("raw page token persisted")
	}
	var complete bool
	var cursor string
	var pages int
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT complete,final_cursor,next_ordinal FROM projection_page_attempts WHERE calendar_id=? AND generation=?`), id, state.Generation).Scan(&complete, &cursor, &pages); err != nil {
		t.Fatal(err)
	}
	if !complete || cursor != "synthetic-final-cursor" || pages != 2 {
		t.Fatal("unexpected staged final state")
	}
	stale := *state
	stale.LeaseOwner = "other"
	if err := s.AppendProjectionPage(ctx, stale, in.AttemptID, 0, "", "Europe/Belgrade", first, now); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale lease: %v", err)
	}
	if err := s.beginProjectionMutation(ctx, id, "page-mutation-0123456789", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, string(first.NextPageToken), "Europe/Belgrade", final, now); !errors.Is(err, ErrProjectionSnapshotStale) {
		t.Fatalf("pending mutation: %v", err)
	}
	if err := s.resolveProjectionMutation(ctx, id, "page-mutation-0123456789"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, string(first.NextPageToken), "Europe/Belgrade", final, now); !errors.Is(err, ErrProjectionSnapshotStale) {
		t.Fatalf("changed epoch: %v", err)
	}
}

func TestProjectionPageStagingRequiresMatchingIncrementalBase(t *testing.T) {
	s, now := newEventCacheStore(t)
	runProjectionPageMatchingIncrementalBase(t, s, "calendar", now)
}

func runProjectionPageMatchingIncrementalBase(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ensureProjectionPageStagingSchema(t, s)
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=? WHERE id=?`), "Europe/Belgrade", id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim %v", err)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`), "base-cursor", id); err != nil {
		t.Fatal(err)
	}
	base := int64(0)
	input := ProjectionSnapshotInput{CalendarID: id, StageID: "base-stage-0123456789", Timezone: "Europe/Belgrade", BaseRevision: &base, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, CompletedAt: now, Cursor: "base-cursor", Payload: []byte(`[]`), Complete: true}
	if err := s.StageProjectionSnapshot(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProjectionSnapshot(ctx, id, input.StageID); err != nil {
		t.Fatal(err)
	}
	attempt := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "incremental-0123456789", Timezone: "Europe/Belgrade", Generation: state.Generation, BaseRevision: 1, BaseCursor: "base-cursor", WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncIncremental}
	if err := s.StartProjectionPageAttempt(ctx, *state, attempt, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("unverified base: %v", err)
	}
	seedVerifiedProjectionSnapshot(t, s, id, 1)
	bad := attempt
	bad.BaseRevision = 0
	if err := s.StartProjectionPageAttempt(ctx, *state, bad, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("missing base: %v", err)
	}
	bad = attempt
	bad.Timezone = "UTC"
	if err := s.StartProjectionPageAttempt(ctx, *state, bad, now); !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("timezone mismatch: %v", err)
	}
	if err := s.StartProjectionPageAttempt(ctx, *state, attempt, now); err != nil {
		t.Fatal(err)
	}
}
