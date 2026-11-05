package storage

import (
	"testing"
	"time"
)

func TestApplyEventSyncPageTxRollbackIncludesFinalCursorAndEvents(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	state, err := s.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockCalendarSyncLease(ctx, s, tx, *state, now); err != nil {
		t.Fatal(err)
	}
	batch := EventSyncBatch{FullSync: true, NextCursor: "next", Upserts: []CachedEventUpsert{{Event: cachedTimedEvent("event", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")}}}
	if err := s.applyEventSyncPageTx(ctx, tx, *state, batch, true, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var cursor, status string
	if err := s.db.QueryRowContext(ctx, `SELECT cursor,status FROM calendar_sync_state WHERE calendar_id='calendar'`).Scan(&cursor, &status); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cached_events WHERE calendar_id='calendar'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if cursor != "" || status != "syncing" || count != 0 {
		t.Fatalf("rollback cursor=%q status=%q count=%d", cursor, status, count)
	}
	if err := s.ApplyEventSyncPage(ctx, *state, batch, true, now); err != nil {
		t.Fatalf("legacy path failed: %v", err)
	}
}
