//go:build integration

package storage

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresSyncControlConcurrent(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	first, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err = first.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runControlConcurrentContract(t, first, second)
}

func TestPostgresStoppedLeaseRemainsUnknown(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	id := fmt.Sprintf("stop-pg-%d", time.Now().UnixNano())
	now := setupControlRule(t, s, id)
	if err = s.EnqueueJob(ctx, Job{ID: id + "job", RuleID: id, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now.Add(time.Second))
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err = s.RequestRulePause(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverStaleJobs(ctx, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.db.QueryRowContext(ctx, s.query(`SELECT state FROM sync_jobs WHERE id=?`), id+"job").Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("state %q %v", state, err)
	}
	if err = s.ArchiveRule(ctx, id, now); !errors.Is(err, ErrSyncControlConflict) {
		t.Fatalf("archive unknown: %v", err)
	}
}
