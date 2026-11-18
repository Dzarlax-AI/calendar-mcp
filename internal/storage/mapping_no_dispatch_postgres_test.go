//go:build integration

package storage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestPostgresNoDispatchRestoreWaitsForJobFence(t *testing.T) {
	raw := os.Getenv("TEST_POSTGRES_URL")
	if raw == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.Path == "" || u.Path == "/" || u.Query().Has("host") || u.Query().Has("hostaddr") || u.Query().Has("service") || os.Getenv("TEST_POSTGRES_DISPOSABLE") != "1" {
		t.Fatal("test requires TEST_POSTGRES_DISPOSABLE=1 and an explicit loopback PostgreSQL database URL")
	}
	ctx := t.Context()
	admin, err := Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("restore_%d", time.Now().UnixNano())
	if _, err = admin.db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.db.ExecContext(cleanup, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()
	q := u.Query()
	q.Set("search_path", schema)
	q.Set("application_name", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, Connection{ID: "c", Provider: "google", AccountFingerprint: "c", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err = s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "c", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.CreateRule(ctx, Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = s.EnqueueJob(ctx, Job{ID: "job", RuleID: "rule", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err = s.UpsertMapping(ctx, Mapping{ID: "mapping", RuleID: "rule", SourceEventID: "event", TargetEventID: "copy", ContentHash: "old", LastSeenAt: now, ReconciliationState: "current"}); err != nil {
		t.Fatal(err)
	}
	values, err := s.ListMappings(ctx, "rule")
	if err != nil || len(values) != 1 {
		t.Fatal(err)
	}
	previous := values[0]
	pending := previous
	pending.ReconciliationState = "pending_update"
	if err = s.UpdateMappingCAS(ctx, pending, pending.Version); err != nil {
		t.Fatal(err)
	}
	pending.Version++
	previous.Version = pending.Version
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE sync_jobs SET lease_until=? WHERE id=?`), now.Add(-time.Minute), job.ID); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err = tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.RestoreMappingAfterNoDispatchCAS(ctx, previous, pending.Version, *job) }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waitForLock:
	for {
		select {
		case err := <-result:
			t.Fatalf("restore returned before waiting for job lock: %v", err)
		case <-deadline.C:
			t.Fatal("restore was not observed waiting for the job lock")
		case <-ticker.C:
			var blocked bool
			err = admin.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
				WHERE application_name=$1 AND wait_event_type='Lock'
				AND $2=ANY(pg_blocking_pids(pid)) AND query LIKE '%SELECT id FROM sync_jobs%FOR UPDATE%')`, schema, blockerPID).Scan(&blocked)
			if err != nil {
				t.Fatal(err)
			}
			if blocked {
				break waitForLock
			}
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrMappingConflict) {
			t.Fatalf("stale lease restored: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restore blocked")
	}
	got, err := s.ListMappings(ctx, "rule")
	if err != nil || len(got) != 1 || got[0].ReconciliationState != "pending_update" || got[0].Version != pending.Version {
		t.Fatalf("lost pending: %+v %v", got, err)
	}
}
