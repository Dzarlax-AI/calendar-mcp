package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func stagingDigest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestRemovalStagingIsLeaseFencedOrderedAndPrivate(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := t.Context()
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim: %v", err)
	}
	runRemovalStagingContract(t, store, *state, now)
}

func runRemovalStagingContract(t *testing.T, store *Store, claimed CalendarSyncState, now time.Time) {
	t.Helper()
	ctx := t.Context()
	state := &claimed
	first := RemovalStagePage{Ordinal: 0, PageHash: stagingDigest("page0"), PreviousTokenHash: stagingDigest(""), NextTokenHash: stagingDigest("p1"), Removals: []calendar.EventSyncRemoval{{ObjectID: "id", Reason: calendar.EventSyncProviderCancellation}}}
	second := RemovalStagePage{Ordinal: 1, PageHash: stagingDigest("page1"), PreviousTokenHash: stagingDigest("p1"), NextTokenHash: stagingDigest("p2")}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{RemovalStage: &second}, false, now); !errors.Is(err, ErrInvalidRemovalStage) {
		t.Fatalf("out of order: %v", err)
	}
	badBatch := EventSyncBatch{RemovalStage: &first, Upserts: []CachedEventUpsert{{Event: calendar.EventV2{ID: "id", CalendarID: "another-calendar"}}}}
	if err := store.ApplyEventSyncPage(ctx, *state, badBatch, false, now); err == nil {
		t.Fatal("invalid cache page accepted")
	}
	var rolledBack int
	if err := store.db.QueryRowContext(ctx, store.query(`SELECT COUNT(*) FROM event_sync_removal_staging WHERE calendar_id=?`), state.CalendarID).Scan(&rolledBack); err != nil || rolledBack != 0 {
		t.Fatalf("staging survived failed page: %d %v", rolledBack, err)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{RemovalStage: &first}, false, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{RemovalStage: &first}, false, now); err != nil {
		t.Fatalf("replay: %v", err)
	}
	changed := first
	changed.PageHash = stagingDigest("different")
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{RemovalStage: &changed}, false, now); !errors.Is(err, ErrInvalidRemovalStage) {
		t.Fatalf("conflicting replay: %v", err)
	}
	stale := *state
	stale.LeaseOwner = "other"
	if err := store.ApplyEventSyncPage(ctx, stale, EventSyncBatch{RemovalStage: &second}, false, now); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale lease: %v", err)
	}
	wrong := second
	wrong.PreviousTokenHash = stagingDigest("wrong")
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{RemovalStage: &wrong}, false, now); !errors.Is(err, ErrInvalidRemovalStage) {
		t.Fatalf("broken token chain: %v", err)
	}
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{RemovalStage: &second}, false, now); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := store.db.QueryRowContext(ctx, store.query(`SELECT COUNT(*) FROM event_sync_removal_staging WHERE calendar_id=?`), state.CalendarID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("staged pages=%d err=%v", n, err)
	}
}
