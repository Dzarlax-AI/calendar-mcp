package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
)

func TestProjectionFinalizerIncrementalSQLite(t *testing.T) {
	s, now := newEventCacheStore(t)
	ensureProjectionPageStagingSchema(t, s)
	runProjectionFinalizerIncremental(t, s, "calendar", now)
}
func runProjectionFinalizerIncremental(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE calendars SET timezone=?,enabled=?,can_read=? WHERE id=?`), "UTC", true, true, id); err != nil {
		t.Fatal(err)
	}
	state, err := s.ClaimDueCalendarSync(ctx, "incremental-finalizer", now, now.Add(time.Hour))
	if err != nil || state == nil || state.CalendarID != id {
		t.Fatalf("claim: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET cursor=? WHERE calendar_id=?`), "base-cursor", id); err != nil {
		t.Fatal(err)
	}
	original := calendar.EventTime{DateTime: now.Add(-10 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}
	moved := calendar.EventV2{ID: "moved", CalendarID: id, Provider: "google", RecurringEventID: "series", OriginalStart: &original, Start: original, End: calendar.EventTime{DateTime: now.Add(10 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	cancelled := calendar.EventV2{ID: "cancelled", CalendarID: id, Provider: "google", Start: original, End: calendar.EventTime{DateTime: now.Add(10 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	for _, event := range []calendar.EventV2{moved, cancelled} {
		if err = s.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
	}
	basePayload, err := encodeProjectionSnapshotPayload([]calendar.EventV2{moved, cancelled}, id)
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := int64(0)
	base := ProjectionSnapshotInput{CalendarID: id, StageID: "incremental-base-0123456789", Timezone: "UTC", BaseRevision: &baseRevision, WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, CompletedAt: now, Cursor: "base-cursor", Payload: basePayload, Complete: true}
	if err = s.StageProjectionSnapshot(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishProjectionSnapshot(ctx, id, base.StageID); err != nil {
		t.Fatal(err)
	}
	seedVerifiedProjectionSnapshot(t, s, id, 1)
	prior, err := json.Marshal([]calendarprojection.RemovalObservation{{Removal: calendar.EventSyncRemoval{ObjectID: "earlier", Reason: calendar.EventSyncProviderCancellation}, Unresolved: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`INSERT INTO projection_revision_provenance(calendar_id,revision,observations) VALUES(?,?,?)`), id, 1, prior); err != nil {
		t.Fatal(err)
	}
	var connection string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT connection_id FROM calendars WHERE id=?`), id).Scan(&connection); err != nil {
		t.Fatal(err)
	}
	target := id + "-retention-target"
	if err = s.UpsertCalendar(ctx, Calendar{ID: target, ConnectionID: connection, ProviderCalendarID: target, Name: target, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	ruleID := id + "-retention-rule"
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM projection_rule_checkpoints WHERE rule_id=?`), ruleID)
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM projection_snapshot_pins WHERE rule_id=?`), ruleID)
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM sync_rules WHERE id=?`), ruleID)
	})
	if err = s.CreateRule(ctx, Rule{ID: ruleID, SourceCalendarID: id, TargetCalendarID: target, State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	pinID := id + "-retention-pin"
	if _, err = s.db.ExecContext(ctx, s.query(`INSERT INTO projection_snapshot_pins(pin_id,calendar_id,revision,rule_id,rule_version,job_id,job_attempt,worker_id,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), pinID, id, 1, ruleID, 1, "synthetic-job", 1, "synthetic-worker", "active", now, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	in := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "incremental-attempt-0123456789", Timezone: "UTC", Generation: state.Generation, GuardEpoch: 0, BaseRevision: 1, BaseCursor: "base-cursor", WindowStart: state.WindowStart, WindowEnd: state.WindowEnd, Mode: calendar.EventSyncIncremental}
	if err = s.StartProjectionPageAttempt(ctx, *state, in, now); err != nil {
		t.Fatal(err)
	}
	oldIdentity, err := calendarprojection.Identity(id, moved)
	if err != nil {
		t.Fatal(err)
	}
	moved.Start = calendar.EventTime{DateTime: now.Add(20 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}
	moved.End = calendar.EventTime{DateTime: now.Add(40 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}
	first := calendar.EventSyncPage{SourceTimeZone: "UTC", NextPageToken: "token", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: moved.ID}, Event: moved}}}
	final := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "new-cursor", DeletedEventIDs: []string{cancelled.ID}, Removals: []calendar.EventSyncRemoval{{ObjectID: cancelled.ID, Reason: calendar.EventSyncProviderCancellation}}}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 0, "", "UTC", first, now); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendProjectionPage(ctx, *state, in.AttemptID, 1, "token", "UTC", final, now); err != nil {
		t.Fatal(err)
	}
	// Corrupt the reason but recompute the page digest, so rejection depends on
	// semantic provenance validation, not merely a mismatching hash.
	var payload []byte
	var hash, prev, next string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT payload,page_hash,previous_token_hash,next_token_hash FROM projection_page_attempt_pages WHERE calendar_id=? AND generation=? AND page_ordinal=?`), id, state.Generation, 1).Scan(&payload, &hash, &prev, &next); err != nil {
		t.Fatal(err)
	}
	malformed := final
	malformed.NextCursor = ""
	malformed.Removals = []calendar.EventSyncRemoval{{ObjectID: cancelled.ID, Reason: "unknown"}}
	badPayload, err := encodeProjectionPagePayload(malformed, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Payload                json.RawMessage
		Previous, Next, Cursor string
	}{badPayload, prev, next, "new-cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE projection_page_attempt_pages SET payload=?,page_hash=? WHERE calendar_id=? AND generation=? AND page_ordinal=?`), badPayload, projectionPageHash(raw), id, state.Generation, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE projection_page_attempts SET total_bytes=total_bytes+? WHERE calendar_id=? AND generation=?`), len(badPayload)-len(payload), id, state.Generation); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.finalizeProjectionTx(ctx, tx, *state, in.AttemptID, now, now.Add(time.Hour))
	_ = tx.Rollback()
	if !errors.Is(err, ErrProjectionPageStage) {
		t.Fatalf("malformed provenance accepted: %v", err)
	}
	var revision int64
	var cursor, status string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT revision FROM projection_mutation_guards WHERE calendar_id=?`), id).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("rollback revision: %v", err)
	}
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT cursor,status FROM calendar_sync_state WHERE calendar_id=?`), id).Scan(&cursor, &status); err != nil || cursor != "base-cursor" || status != "syncing" {
		t.Fatalf("rollback cursor: %v", err)
	}
	var cached int
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM cached_events WHERE calendar_id=? AND deleted=?`), id, false).Scan(&cached); err != nil || cached != 2 {
		t.Fatalf("rollback cached events: %d %v", cached, err)
	}
	if _, err = s.GetProjectionSnapshot(ctx, id, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed provenance published a revision: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE projection_page_attempt_pages SET payload=?,page_hash=? WHERE calendar_id=? AND generation=? AND page_ordinal=?`), payload, hash, id, state.Generation, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE projection_page_attempts SET total_bytes=total_bytes+? WHERE calendar_id=? AND generation=?`), len(payload)-len(badPayload), id, state.Generation); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.finalizeProjectionTx(ctx, tx, *state, in.AttemptID, now, now.Add(time.Hour))
	if err != nil || rev != 2 {
		_ = tx.Rollback()
		t.Fatalf("finalize: %d %v", rev, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetProjectionSnapshot(ctx, id, 2)
	if err != nil || !snap.Verified {
		t.Fatalf("snapshot: %v", err)
	}
	events, err := decodeProjectionSnapshotPayload(snap.Payload, id)
	if err != nil || len(events) != 1 || events[0].ID != moved.ID || events[0].Start.DateTime != moved.Start.DateTime {
		t.Fatalf("moved event: %v", err)
	}
	newIdentity, err := calendarprojection.Identity(id, events[0])
	if err != nil || newIdentity != oldIdentity {
		t.Fatalf("occurrence identity: %v", err)
	}
	var provenance []byte
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT observations FROM projection_revision_provenance WHERE calendar_id=? AND revision=?`), id, 2).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	var observations []calendarprojection.RemovalObservation
	if err = json.Unmarshal(provenance, &observations); err != nil || len(observations) != 2 || observations[0].Removal.ObjectID != "earlier" || !observations[0].Unresolved || observations[1].Unresolved || observations[1].PreviousIdentity == "" || observations[1].Removal.Reason != calendar.EventSyncProviderCancellation {
		t.Fatalf("cancellation provenance: %v", err)
	}
	var removed bool
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT deleted FROM cached_events WHERE calendar_id=? AND event_id=?`), id, cancelled.ID).Scan(&removed); err != nil || !removed {
		t.Fatalf("cancelled cache: %v", err)
	}
	if _, err = s.GetProjectionSnapshot(ctx, id, 1); err != nil {
		t.Fatalf("active pin lost source revision: %v", err)
	}
	// A third run must retain all cancellation observations without hitting the
	// two-revision capacity. The current source cursor is the only input.
	later := now.Add(time.Hour)
	third, err := s.ClaimDueCalendarSync(ctx, "third-worker", later, later.Add(time.Hour))
	if err != nil || third == nil || third.CalendarID != id {
		t.Fatalf("third claim: %v", err)
	}
	thirdAttempt := ProjectionPageAttemptInput{CalendarID: id, AttemptID: "third-attempt-0123456789", Timezone: "UTC", Generation: third.Generation, BaseRevision: 2, BaseCursor: "new-cursor", WindowStart: third.WindowStart, WindowEnd: third.WindowEnd, Mode: calendar.EventSyncIncremental}
	if err = s.StartProjectionPageAttempt(ctx, *third, thirdAttempt, later); err != nil {
		t.Fatal(err)
	}
	thirdPage := calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "third-cursor"}
	if _, err = s.finalizeSourceProjectionPage(ctx, *third, thirdAttempt.AttemptID, 0, "", "UTC", thirdPage, later, later.Add(time.Hour)); !errors.Is(err, ErrProjectionSnapshotLimit) {
		t.Fatalf("active pin capacity: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`UPDATE projection_snapshot_pins SET status=?,released_at=? WHERE pin_id=?`), "released", later, pinID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`INSERT INTO projection_rule_checkpoints(rule_id,checkpoint_version,calendar_id,revision,window_start,window_end,rule_version,job_id,job_attempt,worker_id,pin_id,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), ruleID, 1, id, 1, state.WindowStart, state.WindowEnd, 1, "synthetic-job", 1, "synthetic-worker", pinID, later); err != nil {
		t.Fatal(err)
	}
	if _, err = s.finalizeSourceProjectionPage(ctx, *third, thirdAttempt.AttemptID, 0, "", "UTC", thirdPage, later, later.Add(time.Hour)); !errors.Is(err, ErrProjectionSnapshotLimit) {
		t.Fatalf("checkpoint capacity: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, s.query(`DELETE FROM projection_rule_checkpoints WHERE rule_id=?`), ruleID); err != nil {
		t.Fatal(err)
	}
	thirdRevision, err := s.finalizeSourceProjectionPage(ctx, *third, thirdAttempt.AttemptID, 0, "", "UTC", thirdPage, later, later.Add(time.Hour))
	if err != nil || thirdRevision != 3 {
		t.Fatalf("third finalization: %d %v", thirdRevision, err)
	}
	if _, err = s.GetProjectionSnapshot(ctx, id, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released pin retained old revision: %v", err)
	}
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT observations FROM projection_revision_provenance WHERE calendar_id=? AND revision=?`), id, 3).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	observations = nil
	if err = json.Unmarshal(provenance, &observations); err != nil || len(observations) != 2 || observations[0].Removal.ObjectID != "earlier" || observations[1].Removal.ObjectID != cancelled.ID {
		t.Fatalf("third tombstone history: %v", err)
	}
}
