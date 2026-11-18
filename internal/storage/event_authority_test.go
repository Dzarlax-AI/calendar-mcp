package storage

import (
	"bytes"
	"calendar-mcp/internal/calendar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBeginAuthorizationFencesSyncAndRefresh(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	original, err := store.CredentialSnapshot(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	if err = store.UpsertCachedEvent(ctx, cachedTimedEvent("old", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z"), now); err != nil {
		t.Fatal(err)
	}
	next, err := store.BeginAuthorization(ctx, "connection", []byte("replacement"), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if next.AuthorizationRevision == "" || next.AuthorizationRevision == original.AuthorizationRevision {
		t.Fatal("epoch did not change")
	}
	if err = store.CompareAndSwapCredentials(ctx, original, []byte("stale"), now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale refresh = %v", err)
	}
	if err = store.ApplyEventSyncPage(ctx, *claim, EventSyncBatch{}, true, now.Add(time.Second)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale page = %v", err)
	}
	var n int
	if err = store.db.QueryRowContext(ctx, "SELECT count(*) FROM cached_events").Scan(&n); err != nil || n != 0 {
		t.Fatalf("cached rows %d, %v", n, err)
	}
}

func TestCompleteGoogleAuthorizationFencesAndAllowsEmpty(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	first, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.BeginAuthorization(ctx, "connection", nil, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	staged := []DiscoveredDetailCalendar{{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary", GooglePrimary: true}, AccessRole: "reader"}}
	if err = store.CompleteGoogleAuthorization(ctx, first, staged, now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale complete = %v", err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, second, staged, now); err != nil {
		t.Fatal(err)
	}
	calendars, err := store.ListCalendars(ctx, "connection")
	if err != nil || len(calendars) != 1 {
		t.Fatalf("calendars: %v %v", calendars, err)
	}
	if !calendars[0].CanRead || calendars[0].CanWrite || DetailReceiptRole(calendars[0].DetailReceipt) != "reader" {
		t.Fatalf("published: %+v", calendars[0])
	}
	third, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, third, nil, now); err != nil {
		t.Fatal(err)
	}
	calendars, err = store.ListCalendars(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	if calendars[0].CanRead || calendars[0].DetailReceipt != "" {
		t.Fatal("empty discovery kept grant")
	}
}

func TestWriterTicketCannotRestoreRevokedProjection(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	snapshot, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	entry := DiscoveredDetailCalendar{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snapshot, []DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.CaptureWriterTicket(ctx, "calendar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginAuthorization(ctx, "connection", nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	event := cachedTimedEvent("late", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")
	if err = store.UpsertCachedEventWithTicket(ctx, ticket, event, now.Add(2*time.Second)); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale upsert = %v", err)
	}
	var n int
	if err = store.db.QueryRowContext(ctx, "SELECT count(*) FROM cached_events").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

func TestGoogleRoleObservationClosesOnDowngradeOrMissing(t *testing.T) {
	for _, role := range []string{"reader", ""} {
		t.Run(role, func(t *testing.T) {
			store, now := newEventCacheStore(t)
			ctx := context.Background()
			snap, err := store.BeginAuthorization(ctx, "connection", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			entry := DiscoveredDetailCalendar{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary"}, AccessRole: "owner"}
			if err = store.CompleteGoogleAuthorization(ctx, snap, []DiscoveredDetailCalendar{entry}, now); err != nil {
				t.Fatal(err)
			}
			ticket, err := store.CaptureWriterTicket(ctx, "calendar")
			if err != nil {
				t.Fatal(err)
			}
			if err = store.UpsertCachedEventWithTicket(ctx, ticket, cachedTimedEvent("private", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z"), now); err != nil {
				t.Fatal(err)
			}
			if err = store.ObserveGoogleRole(ctx, ticket, snap.AuthorizationRevision, sha256.Sum256(snap.EncryptedCredentials), role, now); !errors.Is(err, ErrAuthorityChanged) {
				t.Fatalf("observation = %v", err)
			}
			var n int
			if err = store.db.QueryRowContext(ctx, "SELECT count(*) FROM cached_events").Scan(&n); err != nil || n != 0 {
				t.Fatalf("rows=%d err=%v", n, err)
			}
		})
	}
}

func TestLegacyBootstrapClaimIsOneShot(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	markConnectionMigrated(t, store, "connection")
	var wg sync.WaitGroup
	results := make(chan *CredentialSnapshot, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := store.BeginOneLegacyGoogleAuthorization(ctx, now)
			results <- v
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var claimed *CredentialSnapshot
	count := 0
	for result := range results {
		if result != nil {
			claimed = result
			count++
		}
	}
	if count != 1 {
		t.Fatalf("claims=%d", count)
	}
	if err := store.FailAuthorization(ctx, *claimed, now); err != nil {
		t.Fatal(err)
	}
	again, err := store.BeginOneLegacyGoogleAuthorization(ctx, now)
	if err != nil || again != nil {
		t.Fatalf("failed attempt retried: %v %v", again, err)
	}
}

func TestStaleTokenRoleObservationCannotCloseCurrentGrant(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	snap, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	entry := DiscoveredDetailCalendar{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.CaptureWriterTicket(ctx, "calendar")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompareAndSwapCredentials(ctx, snap, []byte("refreshed"), now); err != nil {
		t.Fatal(err)
	}
	if err = store.ObserveGoogleRole(ctx, ticket, snap.AuthorizationRevision, sha256.Sum256(snap.EncryptedCredentials), "reader", now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale role = %v", err)
	}
	calendars, err := store.ListCalendars(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	if !calendars[0].CanRead || DetailReceiptRole(calendars[0].DetailReceipt) != "owner" {
		t.Fatal("stale token closed current grant")
	}
}

func TestAuthDenialDoesNotRevokeRefreshedToken(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	snap, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	entry := DiscoveredDetailCalendar{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.CaptureWriterTicket(ctx, "calendar")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompareAndSwapCredentials(ctx, snap, []byte("new"), now); err != nil {
		t.Fatal(err)
	}
	if err = store.CloseGoogleAccountIfCurrent(ctx, ticket, snap.AuthorizationRevision, sha256.Sum256(snap.EncryptedCredentials), now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale denial = %v", err)
	}
	current, err := store.CredentialSnapshot(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CloseGoogleAccountIfCurrent(ctx, ticket, current.AuthorizationRevision, sha256.Sum256(current.EncryptedCredentials), now); err != nil {
		t.Fatal(err)
	}
	record, err := store.ConnectionByID(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "error" || record.AuthorizationRevision == snap.AuthorizationRevision {
		t.Fatal("current denial did not fence account")
	}
}

func TestValidateAuthorityTicketsRejectsNewKnownDenial(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	snap, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	entry := DiscoveredDetailCalendar{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary"}, AccessRole: "owner"}
	if err = store.CompleteGoogleAuthorization(ctx, snap, []DiscoveredDetailCalendar{entry}, now); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.CaptureWriterTicket(ctx, "calendar")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateAuthorityTickets(ctx, []AuthorityTicket{ticket}); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || state == nil {
		t.Fatalf("claim %v", err)
	}
	if err = store.ParkCalendarSync(ctx, *state, "permission", now); err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateAuthorityTickets(ctx, []AuthorityTicket{ticket}); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("known denial validation = %v", err)
	}
}

func TestCompleteGoogleAuthorizationAllDeniedKeepsEveryReceiptClosed(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	snap, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	entries := []DiscoveredDetailCalendar{
		{Calendar: Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary", GooglePrimary: true}, AccessRole: "freeBusyReader"},
		{Calendar: Calendar{ID: "other", ConnectionID: "connection", ProviderCalendarID: "other", Name: "Other"}, AccessRole: "none"},
	}
	if err = store.CompleteGoogleAuthorization(ctx, snap, entries, now); err != nil {
		t.Fatal(err)
	}
	cals, err := store.ListCalendars(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cals {
		if c.CanRead || c.CanWrite || c.DetailReceipt != "" {
			t.Fatalf("denied source opened: %+v", c)
		}
	}
	record, err := store.ConnectionByID(ctx, "connection")
	if err != nil || record.Status != "connected" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestBeginFencesConsumedRepairAndPurgesQuarantine(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	first, err := store.ClaimDueCalendarSync(ctx, "first", now, now.Add(time.Hour))
	if err != nil || first == nil {
		t.Fatalf("claim %v", err)
	}
	next := now.Add(time.Minute)
	if err = store.ApplyEventSyncPage(ctx, *first, EventSyncBatch{Warnings: []EventSyncWarning{{ObjectID: "broken", ETag: "etag", ErrorCode: "protocol"}}, NextSyncAt: &next}, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ScheduleEventSyncObjectRepair(ctx, "calendar", "broken", "etag", next); err != nil {
		t.Fatal(err)
	}
	repair, err := store.ClaimDueCalendarSync(ctx, "repair", next, next.Add(time.Hour))
	if err != nil || repair == nil {
		t.Fatalf("repair claim %v", err)
	}
	consumed, err := store.ConsumeEventSyncProviderMutationAuthorization(ctx, *repair, "calendar", "broken", "etag", next)
	if err != nil || !consumed {
		t.Fatalf("consume=%v %v", consumed, err)
	}
	if _, err = store.BeginAuthorization(ctx, "connection", nil, next); err != nil {
		t.Fatal(err)
	}
	batch := EventSyncRepairBatch{ObjectID: "broken", ETag: "new", Outcome: calendar.EventSyncObjectProviderCorrected, Upserts: []CachedEventUpsert{{SourceObjectID: "broken", Event: cachedTimedEvent("repaired", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z")}}}
	if err = store.ApplyEventSyncObjectRepair(ctx, *repair, batch, next.Add(time.Second)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("late repair=%v", err)
	}
	for _, table := range []string{"calendar_sync_quarantine", "calendar_sync_objects", "cached_events"} {
		var count int
		if err = store.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d err=%v", table, count, err)
		}
	}
}

func TestLegacyBootstrapPendingRequiresExplicitRecovery(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := context.Background()
	markConnectionMigrated(t, store, "connection")
	first, err := store.BeginOneLegacyGoogleAuthorization(ctx, now)
	if err != nil || first == nil {
		t.Fatalf("first=%v err=%v", first, err)
	}
	// A worker crash leaves the pending attempt untouched on subsequent cycles.
	again, err := store.BeginOneLegacyGoogleAuthorization(ctx, now.Add(time.Hour))
	if err != nil || again != nil {
		t.Fatalf("pending retried: %v %v", again, err)
	}
	replacement, err := store.BeginAuthorization(ctx, "connection", []byte("reconnect"), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.AuthorizationRevision == replacement.AuthorizationRevision {
		t.Fatal("recovery did not change epoch")
	}
	if err = store.CompleteGoogleAuthorization(ctx, *first, nil, now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("crashed worker completed old attempt: %v", err)
	}
	if err = store.CompleteGoogleAuthorization(ctx, replacement, nil, now); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialSnapshotRejectsNonBlobAndOversizedBeforeTransfer(t *testing.T) {
	store, _ := newEventCacheStore(t)
	ctx := context.Background()
	for _, value := range []any{strings.Repeat("é", 9000), bytes.Repeat([]byte{1}, 16385)} {
		if _, err := store.db.ExecContext(ctx, `UPDATE connections SET encrypted_credentials=? WHERE id='connection'`, value); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CredentialSnapshot(ctx, "connection"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid credential snapshot=%v", err)
		}
		record, err := store.ConnectionByID(ctx, "connection")
		if err != nil {
			t.Fatal(err)
		}
		if len(record.EncryptedCredentials) != 0 {
			t.Fatal("oversized or mistyped credentials transferred")
		}
	}
}

func TestValidateAuthorityTicketsBatchPreservesEveryProof(t *testing.T) {
	var specs [][3]string
	for i := 0; i < 16; i++ {
		specs = append(specs, [3]string{fmt.Sprintf("conn-%02d", i), "primary", "google"})
	}
	store, _, ids := newCachedPagesStore(t, specs...)
	ctx := context.Background()
	tickets := make([]AuthorityTicket, len(ids))
	for i, id := range ids {
		var err error
		tickets[i], err = store.CaptureWriterTicket(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(name string, input []AuthorityTicket, valid bool) {
		t.Helper()
		err := store.ValidateAuthorityTickets(ctx, input)
		if valid && err != nil || !valid && !errors.Is(err, ErrAuthorityChanged) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	check("sixteen", tickets, true)
	check("empty", nil, false)
	check("identical duplicates", []AuthorityTicket{tickets[0], tickets[0]}, true)
	bad := append([]AuthorityTicket(nil), tickets...)
	bad[15].DetailReceipt = tickets[0].DetailReceipt
	check("last mismatched receipt", bad, false)
	bad = append([]AuthorityTicket(nil), tickets...)
	bad[15].AuthorizationRevision = tickets[0].AuthorizationRevision
	check("last mismatched revision", bad, false)
	bad = []AuthorityTicket{tickets[0], tickets[0]}
	bad[1].DetailReceipt = tickets[1].DetailReceipt
	check("conflicting duplicate", bad, false)
	bad = append([]AuthorityTicket(nil), tickets...)
	bad[0].DetailReceipt, bad[1].DetailReceipt = bad[1].DetailReceipt, bad[0].DetailReceipt
	check("cross-swapped receipts", bad, false)
	for _, field := range []string{"revision", "receipt", "long receipt", "connection", "calendar"} {
		bad = append([]AuthorityTicket(nil), tickets...)
		switch field {
		case "revision":
			bad[15].AuthorizationRevision = ""
		case "receipt":
			bad[15].DetailReceipt = ""
		case "long receipt":
			bad[15].DetailReceipt = strings.Repeat("x", 1025)
		case "connection":
			bad[15].ConnectionID = ""
		case "calendar":
			bad[15].CalendarID = ""
		}
		check(field, bad, false)
	}
	for _, tc := range []struct {
		name, update, restore string
		args                  []any
	}{
		{"disconnected", `UPDATE connections SET status='error' WHERE id=?`, `UPDATE connections SET status='connected' WHERE id=?`, []any{tickets[15].ConnectionID}},
		{"unreadable", `UPDATE calendars SET can_read=false WHERE id=?`, `UPDATE calendars SET can_read=true WHERE id=?`, []any{ids[15]}},
		{"auth", `UPDATE calendar_sync_state SET last_error_code='auth' WHERE calendar_id=?`, `UPDATE calendar_sync_state SET last_error_code=NULL WHERE calendar_id=?`, []any{ids[15]}},
		{"permission", `UPDATE calendar_sync_state SET last_error_code='permission' WHERE calendar_id=?`, `UPDATE calendar_sync_state SET last_error_code=NULL WHERE calendar_id=?`, []any{ids[15]}},
	} {
		if _, err := store.db.ExecContext(ctx, tc.update, tc.args...); err != nil {
			t.Fatal(err)
		}
		check(tc.name, tickets, false)
		if _, err := store.db.ExecContext(ctx, tc.restore, tc.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM calendar_sync_state WHERE calendar_id=?`, ids[15]); err != nil {
		t.Fatal(err)
	}
	check("missing sync state", tickets, false)
}

func TestUnsuitableLegacyBootstrapDoesNotStarveNextAccount(t *testing.T) {
	for _, bad := range []struct {
		name  string
		value any
	}{{"oversized", bytes.Repeat([]byte{1}, 16385)}, {"wrong-type", "not-a-blob"}} {
		t.Run(bad.name, func(t *testing.T) {
			testUnsuitableLegacyBootstrap(t, newSQLiteStore(t), bad.value)
		})
	}
}

func testUnsuitableLegacyBootstrap(t *testing.T, store *Store, bad any) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	ids := []string{"a-bad", "b-good"}
	for _, id := range ids {
		if err := store.CreateConnection(ctx, Connection{ID: id, Provider: "google", DisplayName: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		markConnectionMigrated(t, store, id)
		if err := store.UpsertCalendar(ctx, Calendar{ID: "google:" + id + ":cal", ConnectionID: id, ProviderCalendarID: "cal", Name: "cal", CanRead: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EnsureCalendarSyncStates(ctx, now, SyncWindow{Start: now, End: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		event := cachedTimedEvent(id, now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
		event.CalendarID, event.Provider = "google:"+id+":cal", "google"
		if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, store.query(`UPDATE connections SET encrypted_credentials=? WHERE id='a-bad'`), bad); err != nil {
		t.Fatal(err)
	}
	other := &Store{db: store.db, dialect: store.dialect}
	first, err := store.BeginOneLegacyGoogleAuthorization(ctx, now)
	if err != nil || first != nil {
		t.Fatalf("first: %v %v", first, err)
	}
	var status, code string
	if err = other.db.QueryRowContext(ctx, `SELECT status,last_error_code FROM connections WHERE id='a-bad'`).Scan(&status, &code); err != nil || status != "error" || code != "auth" {
		t.Fatalf("status %q, code %q: %v", status, code, err)
	}
	var purged, retained int
	if err = other.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cached_events WHERE calendar_id='google:a-bad:cal'`).Scan(&purged); err != nil || purged != 0 {
		t.Fatalf("old projection: %d rows, %v", purged, err)
	}
	if err = other.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cached_events WHERE calendar_id='google:b-good:cal'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("neighbor projection: %d rows, %v", retained, err)
	}
	next, err := other.BeginOneLegacyGoogleAuthorization(ctx, now)
	if err != nil || next == nil || next.ConnectionID != "b-good" {
		t.Fatalf("next: %v %v", next, err)
	}
	reconnect, err := other.BeginAuthorization(ctx, "a-bad", []byte("valid replacement"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.failUnsuitableLegacyGoogle(ctx, "a-bad", now); err != nil {
		t.Fatal(err)
	}
	current, err := other.CredentialSnapshot(ctx, "a-bad")
	if err != nil || current.Status != "pending" || current.AuthorizationRevision != reconnect.AuthorizationRevision {
		t.Fatalf("reconnect fence: status %q, %v", current.Status, err)
	}
}

func markConnectionMigrated(t *testing.T, store *Store, id string) {
	t.Helper()
	if _, err := store.db.ExecContext(t.Context(), store.query(`UPDATE connections SET authorization_revision='' WHERE id=?`), id); err != nil {
		t.Fatal(err)
	}
}

func TestMissingDetailReceiptDoesNotBootstrapAuthorizedGoogleAccount(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := t.Context()
	snapshot, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	primary := Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary"}
	if err := store.CompleteGoogleAuthorization(ctx, snapshot, []DiscoveredDetailCalendar{{Calendar: primary, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	event := cachedTimedEvent("kept", now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
	if err := store.UpsertCachedEvent(ctx, event, now); err != nil {
		t.Fatal(err)
	}
	// A legacy or direct upsert can still leave an ordinary metadata row
	// without proof on an already-authorized connection.
	if err := store.UpsertCalendar(ctx, Calendar{ID: "row", ConnectionID: "connection", ProviderCalendarID: "new", Name: "New", CanRead: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.BeginOneLegacyGoogleAuthorization(ctx, now)
	if err != nil || candidate != nil {
		t.Fatalf("authorized account reconsidered for migration: %v %v", candidate, err)
	}
	current, err := store.CredentialSnapshot(ctx, "connection")
	if err != nil || current.AuthorizationRevision != snapshot.AuthorizationRevision || current.Status != "connected" {
		t.Fatalf("authorization changed: status=%s error=%v", current.Status, err)
	}
	events, _, err := store.ListCachedEvents(ctx, []string{"calendar"}, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil || len(events) != 1 || events[0].ID != "kept" {
		t.Fatalf("sibling cache changed: count=%d error=%v", len(events), err)
	}
}

func TestRegisterDiscoveredGoogleMetadataUsesCurrentCredential(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := t.Context()
	snapshot, err := store.BeginAuthorization(ctx, "connection", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteGoogleAuthorization(ctx, snapshot, nil, now); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.CredentialSnapshot(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte("synthetic refresh")
	if err := store.CompareAndSwapCredentials(ctx, snapshot, replacement, now); err != nil {
		t.Fatal(err)
	}
	item := calendar.Calendar{ID: "new", Name: "New", GoogleDiscoveryCredentialDigest: sha256.Sum256(snapshot.EncryptedCredentials)}
	if err := store.RegisterDiscoveredCalendarWithRevision(ctx, "google@connection", item, snapshot.AuthorizationRevision); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("old credential registered metadata: %v", err)
	}
	item.GoogleDiscoveryCredentialDigest = sha256.Sum256(replacement)
	if err := store.RegisterDiscoveredCalendarWithRevision(ctx, "google@connection", item, snapshot.AuthorizationRevision); err != nil {
		t.Fatal(err)
	}
	calendars, err := store.ListCalendars(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range calendars {
		if entry.ProviderCalendarID == item.ID {
			if !entry.Enabled || entry.CanRead || entry.CanWrite || entry.DetailReceipt != "" {
				t.Fatalf("metadata changed authority or selection: %+v", entry)
			}
			return
		}
	}
	t.Fatal("current discovery metadata missing")
}
