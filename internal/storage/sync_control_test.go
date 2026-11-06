package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setupControlRule(t *testing.T, s *Store, id string) time.Time {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"source", "target"} {
		c := id + suffix
		if err := s.UpsertCalendar(ctx, Calendar{ID: c, ConnectionID: id, ProviderCalendarID: c, Name: c, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateRule(ctx, Rule{ID: id, SourceCalendarID: id + "source", TargetCalendarID: id + "target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM sync_runs WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM sync_jobs WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(cleanup, s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	return now
}

func TestSyncControlStopUnknownAndArchive(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	now := setupControlRule(t, s, "control")
	if err := s.EnqueueJob(ctx, Job{ID: "job", RuleID: "control", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now.Add(time.Second))
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err = s.StartRun(ctx, Run{ID: "control-run", JobID: job.ID, RuleID: job.RuleID, Trigger: "manual", Outcome: "running", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = s.RequestRulePause(ctx, "control", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if requested, err := s.JobStopRequested(ctx, *job); err != nil || !requested {
		t.Fatalf("stop requested=%v err=%v", requested, err)
	}
	if err = s.CheckJobDispatchLease(ctx, *job); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("dispatch allowed: %v", err)
	}
	if status, err := s.RuleControlStatus(ctx, "control"); err != nil || status != "pausing" {
		t.Fatalf("status %q %v", status, err)
	}
	if err = s.ArchiveRule(ctx, "control", now); !errors.Is(err, ErrSyncControlConflict) {
		t.Fatalf("archive running job: %v", err)
	}
	if err = s.EnqueueJob(ctx, Job{ID: "later", RuleID: "control", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); !errors.Is(err, ErrCalendarSyncIneligible) {
		t.Fatalf("enqueue paused: %v", err)
	}
	if _, err = s.RecoverStaleJobs(ctx, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.db.QueryRowContext(ctx, `SELECT state FROM sync_jobs WHERE id='job'`).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("state %q %v", state, err)
	}
	runs, err := s.ListRuns(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].Outcome != "unknown" {
		t.Fatalf("stale run must remain unknown: %+v %v", runs, err)
	}
	if status, err := s.RuleControlStatus(ctx, "control"); err != nil || status != "needs_review" {
		t.Fatalf("status %q %v", status, err)
	}
	if err = s.ArchiveRule(ctx, "control", now); !errors.Is(err, ErrSyncControlConflict) {
		t.Fatalf("archive unknown job: %v", err)
	}
	if err = s.SetRuleState(ctx, "control", "enabled", nil, now); err == nil {
		t.Fatal("unknown attempt re-enabled")
	}
}

func TestSyncControlArchiveRetainsHistoryAndMappings(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	now := setupControlRule(t, s, "archive")
	if err := s.RequestRulePause(ctx, "archive", now); err != nil {
		t.Fatal(err)
	}
	m := Mapping{ID: "mapping", RuleID: "archive", ObjectKind: "event", SourceEventID: "source", TargetEventID: "target", ReconciliationState: "current", LastSeenAt: now}
	if err := s.UpsertMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveRule(ctx, "archive", now); err != nil {
		t.Fatal(err)
	}
	mappings, err := s.ListMappings(ctx, "archive")
	if err != nil || len(mappings) != 1 {
		t.Fatalf("mappings lost: %v %v", mappings, err)
	}
	if err = s.SetRuleState(ctx, "archive", "enabled", nil, now); err == nil {
		t.Fatal("archived rule re-enabled")
	}
}

func TestSyncControlPauseRaceWithEnqueue(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("race%d", i)
		now := setupControlRule(t, s, id)
		var wg sync.WaitGroup
		wg.Add(2)
		errs := make(chan error, 2)
		go func() { defer wg.Done(); errs <- s.RequestRulePause(ctx, id, now) }()
		go func() {
			defer wg.Done()
			err := s.EnqueueJob(ctx, Job{ID: id + "job", RuleID: id, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now})
			if errors.Is(err, ErrCalendarSyncIneligible) {
				err = nil
			}
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var pending int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_jobs WHERE rule_id=? AND state IN ('pending','running')`, id).Scan(&pending); err != nil || pending != 0 {
			t.Fatalf("pending=%d err=%v", pending, err)
		}
	}
}

func runControlConcurrentContract(t *testing.T, first, second *Store) {
	t.Helper()
	ctx := t.Context()
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("concurrent-%d-%d", time.Now().UnixNano(), i)
		now := setupControlRule(t, first, id)
		if err := first.EnqueueJob(ctx, Job{ID: id + "job", RuleID: id, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		var claimed *Job
		var claimErr, pauseErr error
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			claimed, claimErr = first.ClaimJob(ctx, "synthetic", time.Now().UTC())
		}()
		go func() { defer wg.Done(); <-start; pauseErr = second.RequestRulePause(ctx, id, time.Now().UTC()) }()
		close(start)
		wg.Wait()
		if claimErr != nil || pauseErr != nil {
			t.Fatalf("claim=%v pause=%v", claimErr, pauseErr)
		}
		var state string
		var stop bool
		if err := first.db.QueryRowContext(ctx, first.query(`SELECT state,stop_requested FROM sync_jobs WHERE id=?`), id+"job").Scan(&state, &stop); err != nil {
			t.Fatal(err)
		}
		if state == "running" {
			if claimed == nil || !stop {
				t.Fatalf("running without stop request: %s %v", state, stop)
			}
			if err := first.CheckJobDispatchLease(ctx, *claimed); !errors.Is(err, ErrJobLeaseLost) {
				t.Fatalf("post-pause dispatch allowed: %v", err)
			}
		} else if state != "cancelled" {
			t.Fatalf("unexpected state %s", state)
		}
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("stop-race-%d-%d", time.Now().UnixNano(), i)
		now := setupControlRule(t, first, id)
		if err := first.EnqueueJob(ctx, Job{ID: id + "job", RuleID: id, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var claimed *Job
		var claimErr, stopErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; claimed, claimErr = first.ClaimJob(ctx, "worker", time.Now().UTC()) }()
		go func() { defer wg.Done(); <-start; stopErr = second.RequestJobStop(ctx, id+"job", now) }()
		close(start)
		wg.Wait()
		if claimErr != nil || stopErr != nil {
			t.Fatalf("claim=%v stop=%v", claimErr, stopErr)
		}
		var state string
		var requested bool
		if err := first.db.QueryRowContext(ctx, first.query(`SELECT state,stop_requested FROM sync_jobs WHERE id=?`), id+"job").Scan(&state, &requested); err != nil {
			t.Fatal(err)
		}
		if !requested || (state != "running" && state != "cancelled") {
			t.Fatalf("state=%s stop=%v", state, requested)
		}
		if claimed != nil {
			if err := first.CheckJobDispatchLease(ctx, *claimed); !errors.Is(err, ErrJobLeaseLost) {
				t.Fatalf("stopped claim dispatched: %v", err)
			}
		}
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("schedule-race-%d-%d", time.Now().UnixNano(), i)
		now := setupControlRule(t, first, id)
		var wg sync.WaitGroup
		var scheduleErr, pauseErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, scheduleErr = first.ScheduleDueJobs(ctx, now.Add(time.Second)) }()
		go func() { defer wg.Done(); <-start; pauseErr = second.RequestRulePause(ctx, id, now) }()
		close(start)
		wg.Wait()
		if scheduleErr != nil || pauseErr != nil {
			t.Fatalf("schedule=%v pause=%v", scheduleErr, pauseErr)
		}
		var remaining int
		if err := first.db.QueryRowContext(ctx, first.query(`SELECT count(*) FROM sync_jobs WHERE rule_id=? AND state IN ('pending','running') AND kind<>'dry_run'`), id).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("scheduled after pause: %d %v", remaining, err)
		}
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("archive-race-%d-%d", time.Now().UnixNano(), i)
		now := setupControlRule(t, first, id)
		if err := first.RequestRulePause(ctx, id, now); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var archiveErr, enqueueErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; archiveErr = first.ArchiveRule(ctx, id, now) }()
		go func() {
			defer wg.Done()
			<-start
			enqueueErr = second.EnqueueJob(ctx, Job{ID: id + "dry", RuleID: id, Kind: "dry_run", State: "pending", AvailableAt: now, CreatedAt: now})
		}()
		close(start)
		wg.Wait()
		if archiveErr != nil && !errors.Is(archiveErr, ErrSyncControlConflict) {
			t.Fatal(archiveErr)
		}
		if enqueueErr != nil && !errors.Is(enqueueErr, ErrCalendarSyncIneligible) {
			t.Fatal(enqueueErr)
		}
		if archiveErr == nil && enqueueErr == nil {
			t.Fatal("archived a rule with a concurrently queued job")
		}
		if archiveErr != nil && enqueueErr != nil {
			t.Fatal("neither concurrent action made progress")
		}
	}
}

func TestSyncControlConcurrentSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	first, err := Open(t.Context(), "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err = first.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runControlConcurrentContract(t, first, second)
}

func TestSyncControlStopQueuedAndMappingBlock(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	now := setupControlRule(t, s, "queued")
	if err := s.EnqueueJob(ctx, Job{ID: "queued-job", RuleID: "queued", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestJobStop(ctx, "queued-job", now); err != nil {
		t.Fatal(err)
	}
	if job, err := s.ClaimJob(ctx, "worker", now.Add(time.Second)); err != nil || job != nil {
		t.Fatalf("cancelled job claimed: %+v %v", job, err)
	}
	if err := s.RequestRulePause(ctx, "queued", now); err != nil {
		t.Fatal(err)
	}
	m := Mapping{ID: "uncertain", RuleID: "queued", ObjectKind: "event", SourceEventID: "source", TargetEventID: "target", ReconciliationState: "current", LastSeenAt: now}
	if err := s.UpsertMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE event_mappings SET reconciliation_state='pending_update' WHERE id='uncertain'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveRule(ctx, "queued", now); !errors.Is(err, ErrSyncControlConflict) {
		t.Fatalf("pending update not protected: %v", err)
	}
}

func TestStopCompletionDistinguishesDispatchedFromNotDispatched(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(fmt.Sprintf("dispatched=%v", dispatched), func(t *testing.T) {
			s := newSQLiteStore(t)
			ctx := t.Context()
			now := setupControlRule(t, s, "finish")
			if err := s.EnqueueJob(ctx, Job{ID: "job", RuleID: "finish", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			job, err := s.ClaimJob(ctx, "worker", now.Add(time.Second))
			if err != nil || job == nil {
				t.Fatalf("claim %v", err)
			}
			if err = s.StartRun(ctx, Run{ID: "run", JobID: job.ID, RuleID: job.RuleID, Trigger: "manual", Outcome: "running", StartedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err = s.RequestJobStop(ctx, job.ID, now); err != nil {
				t.Fatal(err)
			}
			if err = s.FinishRun(ctx, "run", *job, "failed", now.Add(2*time.Second), Run{ProviderDispatched: dispatched}); err != nil {
				t.Fatal(err)
			}
			runs, err := s.ListRuns(ctx, 10)
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs %v %v", runs, err)
			}
			want := "stopped"
			if dispatched {
				want = "unknown"
			}
			if runs[0].Outcome != want {
				t.Fatalf("outcome %s want %s", runs[0].Outcome, want)
			}
			if dispatched {
				if err = s.EnqueueJob(ctx, Job{ID: "retry", RuleID: "finish", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); !errors.Is(err, ErrCalendarSyncIneligible) {
					t.Fatalf("unknown result retried: %v", err)
				}
				if n, err := s.ScheduleDueJobs(ctx, now.Add(time.Hour)); err != nil || n != 0 {
					t.Fatalf("unknown scheduled %d %v", n, err)
				}
			}
		})
	}
}
