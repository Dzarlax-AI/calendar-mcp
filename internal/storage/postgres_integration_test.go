//go:build integration

package storage

import (
	"context"
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
	rule := Rule{ID: "parallel-claim-rule", SourceCalendarID: "source", TargetCalendarID: "target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
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
