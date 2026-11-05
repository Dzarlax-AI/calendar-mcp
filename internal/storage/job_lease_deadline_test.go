package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSQLiteJobLeaseDeadline(t *testing.T) {
	s := newSQLiteStore(t)
	runJobLeaseDeadlineContract(t, s, fmt.Sprintf("job-lease-%d", time.Now().UnixNano()))
}

func runJobLeaseDeadlineContract(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM sync_runs WHERE job_id=?`), id)
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM sync_jobs WHERE rule_id=?`), id)
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM sync_rules WHERE id=?`), id)
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(ctx, s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	for _, cal := range []string{id + "-source", id + "-target"} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: cal, ConnectionID: id, ProviderCalendarID: cal, Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateRule(ctx, Rule{ID: id, SourceCalendarID: id + "-source", TargetCalendarID: id + "-target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueJob(ctx, Job{ID: id, RuleID: id, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	claimHolder, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.dialect == DialectPostgres {
		_, err = claimHolder.ExecContext(ctx, s.query(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`), id)
	} else {
		_, err = claimHolder.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=state WHERE id=?`), id)
	}
	if err != nil {
		t.Fatal(err)
	}
	startedClaim := make(chan struct{})
	claimResult := make(chan struct {
		job *Job
		err error
	}, 1)
	go func() {
		close(startedClaim)
		job, err := s.ClaimJob(ctx, "worker", now)
		claimResult <- struct {
			job *Job
			err error
		}{job, err}
	}()
	<-startedClaim
	time.Sleep(200 * time.Millisecond)
	if err = claimHolder.Commit(); err != nil {
		t.Fatal(err)
	}
	claim := <-claimResult
	job, err := claim.job, claim.err
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if job.ClaimedAt == nil || job.ClaimedAt.Before(now.Add(100*time.Millisecond)) {
		t.Fatalf("claim did not account for lock wait: %v", job.ClaimedAt)
	}
	getDeadline := func() sql.NullTime {
		t.Helper()
		var d sql.NullTime
		if err := s.db.QueryRowContext(ctx, s.query(`SELECT lease_until FROM sync_jobs WHERE id=?`), id).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := getDeadline(); !d.Valid || d.Time.Before(now.Add(JobLeaseDuration)) || d.Time.After(now.Add(JobLeaseDuration+time.Second)) {
		t.Fatalf("claim deadline: %+v", d)
	}
	if err := s.CheckJobDispatchLease(ctx, *job); err != nil {
		t.Fatalf("valid dispatch fence: %v", err)
	}
	stale := *job
	stale.Attempt++
	if err := s.CheckJobDispatchLease(ctx, stale); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("wrong attempt dispatch: %v", err)
	}
	stale = *job
	stale.ClaimedBy = "other-worker"
	if err := s.CheckJobDispatchLease(ctx, stale); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("wrong owner dispatch: %v", err)
	}
	originalDeadline := getDeadline().Time
	for _, d := range []any{nil, now.Add(-time.Minute)} {
		if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET lease_until=? WHERE id=?`), d, id); err != nil {
			t.Fatal(err)
		}
		if err := s.CheckJobDispatchLease(ctx, *job); !errors.Is(err, ErrJobLeaseLost) {
			t.Fatalf("missing or expired deadline dispatch: %v", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET lease_until=? WHERE id=?`), originalDeadline, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RenewJobLease(ctx, *job, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("renew: %v %v", ok, err)
	}
	if d := getDeadline(); !d.Valid || d.Time.Before(now.Add(time.Minute+JobLeaseDuration)) || d.Time.After(now.Add(time.Minute+JobLeaseDuration+time.Second)) {
		t.Fatalf("renew deadline: %+v", d)
	}
	if ok, err := s.RenewJobLease(ctx, *job, now); err != nil || ok {
		t.Fatalf("backdated renew: %v %v", ok, err)
	}
	deadline := getDeadline().Time
	if err := s.StartRun(ctx, Run{ID: id + "-run", JobID: id, RuleID: id, Trigger: "manual", Outcome: "running", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, id+"-run", *job, "succeeded", deadline, Run{}); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("expired finish: %v", err)
	}
	delay := func() { time.Sleep(200 * time.Millisecond) }
	if ok, err := s.renewJobLease(ctx, *job, deadline.Add(-100*time.Millisecond), delay); err != nil || ok {
		t.Fatalf("renew expired before commit: %v %v", ok, err)
	}
	if got := getDeadline(); !got.Valid || !got.Time.Equal(deadline) {
		t.Fatalf("late renewal did not roll back: %+v", got)
	}
	if err := s.finishRun(ctx, id+"-run", *job, "succeeded", deadline.Add(-100*time.Millisecond), Run{}, delay); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("finish expired before commit: %v", err)
	}
	var jobState, runState string
	if err := s.db.QueryRowContext(ctx, s.query(`SELECT j.state,r.outcome FROM sync_jobs j JOIN sync_runs r ON r.job_id=j.id WHERE j.id=? AND r.id=?`), id, id+"-run").Scan(&jobState, &runState); err != nil || jobState != "running" || runState != "running" {
		t.Fatalf("late finish did not roll back: %s %s %v", jobState, runState, err)
	}
	// The heartbeat's clock sample predates a held database lock. It must
	// account for the wait rather than revive the claim after its deadline.
	holder, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=state WHERE id=?`), id); err != nil {
		t.Fatal(err)
	}
	result := make(chan struct {
		ok  bool
		err error
	}, 1)
	go func() {
		ok, err := s.RenewJobLease(ctx, *job, deadline.Add(-100*time.Millisecond))
		result <- struct {
			ok  bool
			err error
		}{ok, err}
	}()
	time.Sleep(200 * time.Millisecond)
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	lockedRenew := <-result
	if lockedRenew.err != nil || lockedRenew.ok {
		t.Fatalf("lock-delayed renew: %v %v", lockedRenew.ok, lockedRenew.err)
	}
	holder, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.ExecContext(ctx, s.query(`UPDATE sync_jobs SET state=state WHERE id=?`), id); err != nil {
		t.Fatal(err)
	}
	finishResult := make(chan error, 1)
	go func() {
		finishResult <- s.FinishRun(ctx, id+"-run", *job, "succeeded", deadline.Add(-100*time.Millisecond), Run{})
	}()
	time.Sleep(200 * time.Millisecond)
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-finishResult; !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("lock-delayed finish: %v", err)
	}
	if ok, err := s.RenewJobLease(ctx, *job, deadline); err != nil || ok {
		t.Fatalf("expired renew: %v %v", ok, err)
	}
	if n, err := s.RecoverStaleJobs(ctx, deadline, deadline.Add(-time.Second)); err != nil || n != 0 {
		t.Fatalf("early recovery: %d %v", n, err)
	}
	if n, err := s.RecoverStaleJobs(ctx, deadline, deadline); err != nil || n != 1 {
		t.Fatalf("expired recovery: %d %v", n, err)
	}
	newJob, err := s.ClaimJob(ctx, "next-worker", deadline)
	if err != nil || newJob == nil || newJob.Attempt != job.Attempt+1 {
		t.Fatalf("reclaim: %v", err)
	}
	if ok, err := s.RenewJobLease(ctx, *job, deadline.Add(time.Second)); err != nil || ok {
		t.Fatalf("old owner renewed: %v %v", ok, err)
	}
	// A migration-era claim without a stored deadline remains untrusted even
	// when the legacy heartbeat runs. It can still be recovered as before.
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_jobs SET lease_until=NULL WHERE id=?`), id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RenewJobLease(ctx, *newJob, deadline.Add(time.Second)); err != nil || !ok {
		t.Fatalf("legacy renew: %v %v", ok, err)
	}
	if d := getDeadline(); d.Valid {
		t.Fatalf("legacy claim gained trusted deadline: %+v", d)
	}
	if n, err := s.RecoverStaleJobs(ctx, deadline.Add(2*time.Second), deadline.Add(2*time.Second)); err != nil || n != 1 {
		t.Fatalf("legacy recovery: %d %v", n, err)
	}
}
