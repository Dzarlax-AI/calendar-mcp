package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNonGoogleDiscoveryFencesAndRotatesOnlyChangedSource(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := context.Background()
	// Fixture's provider is Google; use a separate synthetic Apple connection.
	if err := s.CreateConnection(ctx, Connection{ID: "apple", Provider: "apple", DisplayName: "Apple", Status: "connected", EncryptedCredentials: []byte("x"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) Calendar {
		return Calendar{ID: "apple:" + id, ConnectionID: "apple", ProviderCalendarID: id, Name: id, CanRead: true, DiscoveredAt: now}
	}
	a, b := mk("a"), mk("b")
	if err := s.UpsertCalendar(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCalendar(ctx, b); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListCalendars(ctx, "apple")
	if err != nil || len(before) != 2 {
		t.Fatalf("before: %v %v", before, err)
	}
	snap, err := s.CredentialSnapshot(ctx, "apple")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteNonGoogleAuthorization(ctx, snap, []Calendar{b}, "fingerprint", "Apple", now); err != nil {
		t.Fatal(err)
	}
	after, err := s.ListCalendars(ctx, "apple")
	if err != nil {
		t.Fatal(err)
	}
	if after[0].CanRead || after[0].DetailReceipt == before[0].DetailReceipt || after[1].DetailReceipt != before[1].DetailReceipt {
		t.Fatalf("source receipts before=%+v after=%+v", before, after)
	}
	if err = s.UpsertCalendar(ctx, a); err != nil {
		t.Fatal(err)
	}
	restored, err := s.ListCalendars(ctx, "apple")
	if err != nil || restored[0].DetailReceipt == after[0].DetailReceipt || restored[1].DetailReceipt != after[1].DetailReceipt {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
	stale, err := s.BeginAuthorization(ctx, "apple", []byte("new"), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginAuthorization(ctx, "apple", []byte("newer"), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteNonGoogleAuthorization(ctx, stale, []Calendar{a}, "stale", "Stale", now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale completion: %v", err)
	}
}

func TestNonGoogleSourceRevocationPurgesAndFencesLease(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE connections SET provider='apple' WHERE id='connection'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calendar_sync_state SET last_success_at=?,next_sync_at=? WHERE calendar_id='calendar'`, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Make the seeded lease claimable; its next run is moved to the future below.
	if _, err := s.db.ExecContext(ctx, `UPDATE calendar_sync_state SET next_sync_at=? WHERE calendar_id='calendar'`, now); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCachedEvent(ctx, cachedTimedEvent("old", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z"), now); err != nil {
		t.Fatal(err)
	}
	lease, err := s.ClaimDueCalendarSync(ctx, "worker", now, now.Add(time.Minute))
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	c := Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary", CanRead: false, DiscoveredAt: now}
	if err := s.UpsertCalendar(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyEventSyncPage(ctx, *lease, EventSyncBatch{}, true, now.Add(time.Second)); !errors.Is(err, ErrCalendarSyncLeaseLost) {
		t.Fatalf("stale writer: %v", err)
	}
	var lastSuccess *time.Time
	var next time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT last_success_at,next_sync_at FROM calendar_sync_state WHERE calendar_id='calendar'`).Scan(&lastSuccess, &next); err != nil || lastSuccess != nil || !next.Equal(now) {
		t.Fatalf("freshness success=%v next=%v err=%v", lastSuccess, next, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cached_events WHERE calendar_id='calendar'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cached rows=%d err=%v", count, err)
	}
}

func TestLegacyPermissionProjectionPurgedBeforeRecovery(t *testing.T) {
	for _, status := range []string{"parked", "failed", "ready"} {
		for _, recovery := range []string{"schedule", "reauthorize"} {
			t.Run(status+"/"+recovery, func(t *testing.T) {
				s, now := newEventCacheStore(t)
				ctx := t.Context()
				if _, err := s.db.ExecContext(ctx, `UPDATE connections SET provider='apple' WHERE id='connection'`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE calendar_sync_state SET status=?,last_error_code='permission',cursor='legacy',last_success_at=? WHERE calendar_id='calendar'`, status, now); err != nil {
					t.Fatal(err)
				}
				if err := s.UpsertCachedEvent(ctx, cachedTimedEvent("old", "2026-08-22T10:00:00Z", "2026-08-22T11:00:00Z"), now); err != nil {
					t.Fatal(err)
				}
				if recovery == "schedule" {
					if err := s.ScheduleCalendarSync(ctx, "calendar", now, false); err != nil {
						t.Fatal(err)
					}
				} else {
					snap, err := s.CredentialSnapshot(ctx, "connection")
					if err != nil {
						t.Fatal(err)
					}
					c := Calendar{ID: "calendar", ConnectionID: "connection", ProviderCalendarID: "primary", Name: "Primary", CanRead: true, DiscoveredAt: now}
					if err = s.CompleteNonGoogleAuthorization(ctx, snap, []Calendar{c}, "fixture", "Apple", now); err != nil {
						t.Fatal(err)
					}
				}
				var count int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cached_events WHERE calendar_id='calendar'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("cached rows=%d err=%v", count, err)
				}
				var cursor, syncStatus string
				var success *time.Time
				if err := s.db.QueryRowContext(ctx, `SELECT cursor,status,last_success_at FROM calendar_sync_state WHERE calendar_id='calendar'`).Scan(&cursor, &syncStatus, &success); err != nil || cursor != "" || syncStatus != "pending" || success != nil {
					t.Fatalf("cursor=%q status=%q success=%v err=%v", cursor, syncStatus, success, err)
				}
			})
		}
	}
}
