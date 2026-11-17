package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteNumericDSNStorageContract(t *testing.T) {
	for _, format := range []string{"unix", "unix_milli", "unix_micro", "unix_nano"} {
		for _, convert := range []string{"false", "true"} {
			t.Run(format+"/"+convert, func(t *testing.T) {
				ctx := t.Context()
				store, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "numeric.db")+"?_time_integer_format="+format+"&_inttotime="+convert)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err := store.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				runStorageContract(t, store)
			})
		}
	}
}

func TestSQLiteNumericDSNEarlyTimestamp(t *testing.T) {
	for _, format := range []string{"unix", "unix_milli", "unix_micro", "unix_nano"} {
		for _, convert := range []string{"false", "true"} {
			t.Run(format+"/"+convert, func(t *testing.T) {
				ctx := t.Context()
				store, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "early.db")+"?_time_integer_format="+format+"&_inttotime="+convert)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err := store.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				early := time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)
				if err := store.CreateConnection(ctx, Connection{ID: "account", Provider: "apple", Status: "connected", DisplayName: "Synthetic", ScopesJSON: "[]", EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, CreatedAt: early, UpdatedAt: early, LastVerifiedAt: &early}); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"a", "b"} {
					if err := store.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: early}); err != nil {
						t.Fatal(err)
					}
				}
				rule := Rule{ID: "rule", SourceCalendarID: "a", TargetCalendarID: "b", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", NextRunAt: &early, CreatedAt: early, UpdatedAt: early}
				if err := store.CreateRule(ctx, rule); err != nil {
					t.Fatal(err)
				}
				connections, err := store.ListConnections(ctx)
				if err != nil || len(connections) != 1 {
					t.Fatalf("connections: %v %v", connections, err)
				}
				if !connections[0].CreatedAt.Equal(early) || connections[0].LastVerifiedAt == nil || !connections[0].LastVerifiedAt.Equal(early) {
					t.Fatalf("connection times: %+v", connections[0])
				}
				calendars, err := store.ListAllCalendars(ctx)
				if err != nil || len(calendars) != 2 || !calendars[0].DiscoveredAt.Equal(early) {
					t.Fatalf("calendars: %+v %v", calendars, err)
				}
				rules, err := store.ListRules(ctx)
				if err != nil || len(rules) != 1 || rules[0].NextRunAt == nil || !rules[0].NextRunAt.Equal(early) || !rules[0].CreatedAt.Equal(early) {
					t.Fatalf("rules: %+v %v", rules, err)
				}
				got, err := store.RuleByID(ctx, "rule")
				if err != nil || !got.UpdatedAt.Equal(early) {
					t.Fatalf("rule: %+v %v", got, err)
				}
			})
		}
	}
}

func TestSQLiteNumericDSNMixedLegacySyncAndRunOrder(t *testing.T) {
	for _, format := range []string{"unix", "unix_milli", "unix_micro", "unix_nano"} {
		for _, convert := range []string{"false", "true"} {
			t.Run(format+"/"+convert, func(t *testing.T) {
				ctx := t.Context()
				store, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "mixed.db")+"?_time_integer_format="+format+"&_inttotime="+convert+"&_timezone=Europe%2FBelgrade")
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err := store.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
				if err := store.CreateConnection(ctx, Connection{ID: "account", Provider: "apple", Status: "connected", DisplayName: "Synthetic", ScopesJSON: "[]", EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"a", "b"} {
					if err := store.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
						t.Fatal(err)
					}
				}
				window := SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
				if err := store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
					t.Fatal(err)
				}
				// Text timestamps mimic prior default-driver rows. The explicit offset
				// and offset-free value represent expired times in the configured zone.
				if _, err := store.db.ExecContext(ctx, `UPDATE calendar_sync_state SET next_sync_at='2026-01-10 12:59:58', lease_until='2026-01-10 13:59:59 +0200 EET', status='syncing', lease_owner='old' WHERE calendar_id='a'`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE calendar_sync_state SET next_sync_at=?, lease_until=NULL WHERE calendar_id='b'`, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				claim, err := store.ClaimDueCalendarSync(ctx, "new", now, now.Add(time.Minute))
				if err != nil || claim == nil || claim.CalendarID != "a" {
					t.Fatalf("claim: %+v %v", claim, err)
				}
				if err := store.RenewCalendarSyncLease(ctx, *claim, now, now.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err := store.ApplyEventSyncPage(ctx, *claim, EventSyncBatch{}, true, now); err != nil {
					t.Fatal(err)
				}
				rule := Rule{ID: "rule", SourceCalendarID: "a", TargetCalendarID: "b", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
				if err := store.CreateRule(ctx, rule); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"old", "new"} {
					if err := store.EnqueueJob(ctx, Job{ID: id, RuleID: "rule", Kind: "dry_run", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
						t.Fatal(err)
					}
					if err := store.StartRun(ctx, Run{ID: id, JobID: id, RuleID: "rule", Trigger: "dry_run", Outcome: "running", StartedAt: now, DryRun: true}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE sync_runs SET started_at='2026-01-10 12:59:59' WHERE id='old'`); err != nil {
					t.Fatal(err)
				}
				runs, err := store.ListRuns(ctx, 10)
				if err != nil || len(runs) != 2 || runs[0].ID != "new" {
					t.Fatalf("runs: %+v %v", runs, err)
				}
			})
		}
	}
}

func TestSQLiteMixedInstantKeyPrecisionAndInvalidInput(t *testing.T) {
	store, err := Open(t.Context(), "sqlite://"+filepath.Join(t.TempDir(), "precision.db")+"?_time_integer_format=unix_nano&_timezone=Europe%2FBelgrade")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Equal UTC instants despite offsets, with a one-nanosecond boundary.
	var less, equal bool
	err = store.db.QueryRowContext(t.Context(), `SELECT calendar_instant_key('2026-01-10 14:00:00.000000001 +0200 EET',1000000000,'Europe/Belgrade') < calendar_instant_key('2026-01-10T12:00:00.000000002Z',1000000000,'Europe/Belgrade'), calendar_instant_key('2026-01-10 13:00:00',1000000000,'Europe/Belgrade') = calendar_instant_key('2026-01-10T12:00:00Z',1000000000,'Europe/Belgrade')`).Scan(&less, &equal)
	if err != nil || !less || !equal {
		t.Fatalf("comparison less=%v equal=%v err=%v", less, equal, err)
	}
	var key string
	if err := store.db.QueryRowContext(t.Context(), `SELECT calendar_instant_key('invalid',1000000000,'Europe/Belgrade')`).Scan(&key); err == nil {
		t.Fatal("invalid timestamp must fail closed")
	}
}
