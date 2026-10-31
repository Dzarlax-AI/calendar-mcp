package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAppleAuthRecoveryCadence(t *testing.T) {
	store, now := newEventCacheStore(t)
	ctx := t.Context()
	if _, err := store.db.ExecContext(t.Context(), "UPDATE connections SET provider='apple' WHERE id='connection'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), "UPDATE calendar_sync_state SET cursor='saved-cursor' WHERE calendar_id='calendar'"); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ConnectionByID(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "first", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	parkedAt := now.Add(time.Minute)
	for attempt := 0; attempt < 2; attempt++ {
		if err := store.ParkCalendarSync(ctx, *state, "auth", parkedAt); err != nil {
			t.Fatal(err)
		}
		retryAt := parkedAt.Add(15 * time.Minute)
		_, sources, err := store.ListCachedEvents(ctx, []string{"calendar"}, now, now.Add(time.Hour))
		if err != nil || len(sources) != 1 || sources[0].Status != "parked" || sources[0].ErrorCode != "auth" || sources[0].NextSyncAt == nil || !sources[0].NextSyncAt.Equal(retryAt) {
			t.Fatalf("sources=%#v err=%v", sources, err)
		}
		early, err := store.ClaimDueCalendarSync(ctx, "early", retryAt.Add(-time.Nanosecond), retryAt.Add(time.Hour))
		if err != nil || early != nil {
			t.Fatalf("early=%#v err=%v", early, err)
		}
		state, err = store.ClaimDueCalendarSync(ctx, "recheck", retryAt, retryAt.Add(time.Hour))
		if err != nil || state == nil || state.Cursor != "saved-cursor" {
			t.Fatalf("recheck=%#v err=%v", state, err)
		}
		duplicate, err := store.ClaimDueCalendarSync(ctx, "duplicate", retryAt, retryAt.Add(time.Hour))
		if err != nil || duplicate != nil {
			t.Fatalf("duplicate=%#v err=%v", duplicate, err)
		}
		if err := store.ScheduleCalendarSync(ctx, "calendar", retryAt, false); !errors.Is(err, ErrCalendarSyncActive) {
			t.Fatalf("manual refresh overwrote live lease: %v", err)
		}
		parkedAt = retryAt.Add(time.Minute)
	}
	next := parkedAt.Add(5 * time.Minute)
	if err := store.ApplyEventSyncPage(ctx, *state, EventSyncBatch{NextCursor: "fresh-cursor", NextSyncAt: &next}, true, parkedAt); err != nil {
		t.Fatal(err)
	}
	var status, cursor string
	var errorCode sql.NullString
	if err := store.db.QueryRowContext(t.Context(), "SELECT status,cursor,last_error_code FROM calendar_sync_state").Scan(&status, &cursor, &errorCode); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || cursor != "fresh-cursor" || errorCode.Valid {
		t.Fatalf("success status=%s cursor=%s error=%v", status, cursor, errorCode)
	}
	final, err := store.ConnectionByID(ctx, "connection")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(initial.EncryptedCredentials, final.EncryptedCredentials) || initial.CredentialVersion != final.CredentialVersion {
		t.Fatal("recovery changed saved credentials")
	}
}

func TestAutomaticRecoveryOnlyForReadableConnectedAppleAuth(t *testing.T) {
	for _, tc := range []struct {
		provider, code, status string
		readable, want         bool
	}{
		{"apple", "auth", "connected", true, true},
		{"google", "auth", "connected", true, false},
		{"microsoft", "auth", "connected", true, false},
		{"apple", "permission", "connected", true, false},
		{"apple", "protocol", "connected", true, false},
		{"apple", "unsupported", "connected", true, false},
		{"apple", "auth", "disconnected", true, false},
		{"apple", "auth", "connected", false, false},
	} {
		t.Run(tc.provider+"/"+tc.code+"/"+tc.status+"/"+map[bool]string{true: "readable", false: "unreadable"}[tc.readable], func(t *testing.T) {
			store, now := newEventCacheStore(t)
			if _, err := store.db.ExecContext(t.Context(), "UPDATE connections SET provider=?,status=?", tc.provider, tc.status); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(t.Context(), "UPDATE calendars SET can_read=?", tc.readable); err != nil {
				t.Fatal(err)
			}
			// Simulate a parked state written by the previous release: no future retry deadline.
			if _, err := store.db.ExecContext(t.Context(), "UPDATE calendar_sync_state SET status='parked',last_error_code=?,next_sync_at=?", tc.code, now); err != nil {
				t.Fatal(err)
			}
			state, err := store.ClaimDueCalendarSync(t.Context(), "worker", now.Add(time.Hour), now.Add(2*time.Hour))
			if err != nil || (state != nil) != tc.want {
				t.Fatalf("claim=%#v err=%v want=%v", state, err, tc.want)
			}
		})
	}
}

func TestWindowRebaseDoesNotReactivateParkedSources(t *testing.T) {
	for _, code := range []string{"auth", "permission", "protocol", "unsupported"} {
		t.Run(code, func(t *testing.T) {
			store, now := newEventCacheStore(t)
			if _, err := store.db.ExecContext(t.Context(), "UPDATE connections SET provider='apple'"); err != nil {
				t.Fatal(err)
			}
			retryAt := now.Add(15 * time.Minute)
			if _, err := store.db.ExecContext(t.Context(), "UPDATE calendar_sync_state SET status='parked',last_error_code=?,next_sync_at=?", code, retryAt); err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureCalendarSyncStates(t.Context(), now.Add(time.Minute), SyncWindow{Start: now, End: now.Add(24 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			var status, gotCode string
			var next time.Time
			if err := store.db.QueryRowContext(t.Context(), "SELECT status,last_error_code,next_sync_at FROM calendar_sync_state").Scan(&status, &gotCode, &next); err != nil {
				t.Fatal(err)
			}
			if status != "parked" || gotCode != code || !next.Equal(retryAt) {
				t.Fatalf("status=%s code=%s next=%s", status, gotCode, next)
			}
		})
	}
}

func TestAppleAuthDeadlineSurvivesReopen(t *testing.T) {
	store, now := newEventCacheStore(t)
	if _, err := store.db.ExecContext(t.Context(), "UPDATE connections SET provider='apple'"); err != nil {
		t.Fatal(err)
	}
	state, err := store.ClaimDueCalendarSync(t.Context(), "first", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim=%#v err=%v", state, err)
	}
	if err := store.ParkCalendarSync(t.Context(), *state, "auth", now); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := store.db.QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("expected file fixture: %q", path)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	early, err := reopened.ClaimDueCalendarSync(t.Context(), "early", now.Add(time.Minute), now.Add(time.Hour))
	if err != nil || early != nil {
		t.Fatalf("reopened early=%#v err=%v", early, err)
	}
	due, err := reopened.ClaimDueCalendarSync(t.Context(), "due", now.Add(15*time.Minute), now.Add(time.Hour))
	if err != nil || due == nil {
		t.Fatalf("reopened due=%#v err=%v", due, err)
	}
}
