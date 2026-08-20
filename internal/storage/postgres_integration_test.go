//go:build integration

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresStorageContract(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	store, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runStorageContract(t, store)
	testPostgresClaimsOnlyOneJobPerRule(t, databaseURL, store)
	testPostgresFinishAndRecoveryUseConsistentLockOrder(t, databaseURL, store)
}

func testPostgresClaimsOnlyOneJobPerRule(t *testing.T, databaseURL string, first *Store) {
	t.Helper()
	ctx := context.Background()
	second, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	now := time.Now().UTC()
	rule := Rule{ID: "parallel-claim-rule", SourceCalendarID: "microsoft:ms:source-provider", TargetCalendarID: "google:google:target-provider", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := first.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	for iteration := 0; iteration < 20; iteration++ {
		ids := []string{fmt.Sprintf("parallel-a-%d", iteration), fmt.Sprintf("parallel-b-%d", iteration)}
		for _, id := range ids {
			if err := first.EnqueueJob(ctx, Job{ID: id, RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		results := make(chan *Job, 2)
		errorsFound := make(chan error, 2)
		var workers sync.WaitGroup
		for workerNumber, candidateStore := range []*Store{first, second} {
			workers.Add(1)
			go func(worker int, candidate *Store) {
				defer workers.Done()
				<-start
				job, claimErr := candidate.ClaimJob(ctx, fmt.Sprintf("worker-%d", worker), now)
				if claimErr != nil {
					errorsFound <- claimErr
					return
				}
				results <- job
			}(workerNumber, candidateStore)
		}
		close(start)
		workers.Wait()
		close(results)
		close(errorsFound)
		for claimErr := range errorsFound {
			t.Fatal(claimErr)
		}
		claimed := 0
		for job := range results {
			if job != nil {
				claimed++
			}
		}
		if claimed != 1 {
			t.Fatalf("iteration %d claimed %d jobs for one rule, want 1", iteration, claimed)
		}
		if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM sync_jobs WHERE id IN (?, ?)`), ids[0], ids[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func testPostgresFinishAndRecoveryUseConsistentLockOrder(t *testing.T, databaseURL string, first *Store) {
	t.Helper()
	ctx := context.Background()
	second, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	now := time.Now().UTC()
	rule := Rule{ID: "finish-recovery-rule", SourceCalendarID: "microsoft:ms:source-provider", TargetCalendarID: "google:google:target-provider", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := first.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	for iteration := 0; iteration < 20; iteration++ {
		jobID := fmt.Sprintf("finish-recovery-job-%d", iteration)
		runID := fmt.Sprintf("finish-recovery-run-%d", iteration)
		if err := first.EnqueueJob(ctx, Job{ID: jobID, RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		claimed, err := first.ClaimJob(ctx, "race-worker", now)
		if err != nil || claimed == nil || claimed.ID != jobID {
			t.Fatalf("iteration %d claim = %#v, err = %v", iteration, claimed, err)
		}
		if err := first.StartRun(ctx, Run{ID: runID, JobID: jobID, RuleID: rule.ID, Trigger: "manual", Outcome: "running", StartedAt: now}); err != nil {
			t.Fatal(err)
		}

		runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		start := make(chan struct{})
		finishDone := make(chan error, 1)
		recoverDone := make(chan struct {
			count int
			err   error
		}, 1)
		go func(job Job) {
			<-start
			finishDone <- first.FinishRun(runCtx, runID, job, "succeeded", now.Add(2*time.Minute), Run{})
		}(*claimed)
		go func() {
			<-start
			count, recoverErr := second.RecoverStaleJobs(runCtx, now.Add(time.Minute), now.Add(2*time.Minute))
			recoverDone <- struct {
				count int
				err   error
			}{count: count, err: recoverErr}
		}()
		close(start)
		finishErr := <-finishDone
		recovery := <-recoverDone
		cancel()
		if recovery.err != nil {
			t.Fatalf("iteration %d recovery error: %v", iteration, recovery.err)
		}
		if finishErr == nil && recovery.count != 0 {
			t.Fatalf("iteration %d finished job was also recovered", iteration)
		}
		if errors.Is(finishErr, ErrJobLeaseLost) && recovery.count != 1 {
			t.Fatalf("iteration %d lost lease without recovery", iteration)
		}
		if finishErr != nil && !errors.Is(finishErr, ErrJobLeaseLost) {
			t.Fatalf("iteration %d finish error: %v", iteration, finishErr)
		}
		if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM sync_runs WHERE id=?`), runID); err != nil {
			t.Fatal(err)
		}
		if _, err := first.db.ExecContext(ctx, first.query(`DELETE FROM sync_jobs WHERE id=?`), jobID); err != nil {
			t.Fatal(err)
		}
	}
}
