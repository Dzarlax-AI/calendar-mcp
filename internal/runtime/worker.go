package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/eventsync"
	providerfactory "calendar-mcp/internal/providers"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/syncengine"
)

var errWorkerNotConfigured = errors.New("worker storage is not configured")

const workerLease = storage.JobLeaseDuration
const workerHeartbeat = workerLease / 3

type providerBuilder interface {
	Build(context.Context) ([]calendar.Provider, error)
}

func Worker(ctx context.Context) error {
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		return errWorkerNotConfigured
	}
	if cfg.EventReadModelEnabled {
		if err := cfg.ValidateEventReadModel(); err != nil {
			return fmt.Errorf("event read-model configuration: %w", err)
		}
	}
	cipher, err := credentials.NewCipher(cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("credential encryption: %w", err)
	}
	store, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.CheckSchema(ctx); err != nil {
		return err
	}
	store.SetArtifactCipher(cipher)
	var listenConfig net.ListenConfig
	healthListener, err := listenConfig.Listen(ctx, "tcp", cfg.WorkerHealthAddr)
	if err != nil {
		return fmt.Errorf("listen for worker health: %w", err)
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	healthServer := &http.Server{Handler: healthMux, ReadHeaderTimeout: 5 * time.Second}
	healthErrors := make(chan error, 1)
	go func() {
		if serveErr := healthServer.Serve(healthListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			healthErrors <- serveErr
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = healthServer.Shutdown(shutdownCtx)
	}()
	factory := providerfactory.NewFactory(cfg, store, connections.New(store, cipher))
	waitCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Println("calendar worker ready")
	if err := runWorkerCycleWithConfig(waitCtx, store, factory, cfg, time.Now().UTC()); err != nil {
		log.Printf("calendar worker cycle failed: category=%T", err)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := runWorkerCycleWithConfig(waitCtx, store, factory, cfg, time.Now().UTC()); err != nil {
				log.Printf("calendar worker cycle failed: category=%T", err)
			}
		case <-waitCtx.Done():
			return nil
		case healthErr := <-healthErrors:
			return fmt.Errorf("worker health server: %w", healthErr)
		}
	}
}

func runWorkerCycle(ctx context.Context, store *storage.Store, factory providerBuilder, now time.Time) error {
	return runWorkerCycleWithConfig(ctx, store, factory, config.Load(), now)
}

// runWorkerCycleWithConfig gives the worker's two queues a fixed ordering:
// at most one read-model claim is processed, then every due rule job is
// drained. A slow or failing calendar therefore cannot starve rule syncing.
func runWorkerCycleWithConfig(ctx context.Context, store *storage.Store, factory providerBuilder, cfg *config.Config, now time.Time) error {
	if cfg == nil {
		return errors.New("worker configuration is required")
	}
	if cfg.EventReadModelEnabled {
		if err := cfg.ValidateEventReadModel(); err != nil {
			return fmt.Errorf("event read-model configuration: %w", err)
		}
	}
	if err := store.DeleteExpiredRawEventArtifacts(ctx, now); err != nil {
		return err
	}
	if err := store.ExpireSyncRunPreviews(ctx, now); err != nil {
		return err
	}
	if err := runOneEventSync(ctx, store, factory, cfg, now); err != nil {
		// EventSyncError.Error exposes only bounded provider status/reason fields;
		// it deliberately excludes response bodies, cursors, tokens, and payloads.
		log.Printf("calendar event read-model sync failed: error=%v", err)
	}
	if _, err := store.RecoverStaleJobs(ctx, now.Add(-workerLease), now); err != nil {
		return err
	}
	if _, err := store.ScheduleDueJobs(ctx, now); err != nil {
		return err
	}
	workerID := "worker-" + uuid.NewString()
	for {
		job, err := store.ClaimJob(ctx, workerID, time.Now().UTC())
		if err != nil {
			return err
		}
		if job == nil {
			return nil
		}
		if err := executeJob(ctx, store, factory, *job, now); err != nil {
			log.Printf("calendar sync job failed: job_id=%s category=%T", job.ID, err)
		}
	}
}

func runOneEventSync(ctx context.Context, store *storage.Store, factory providerBuilder, cfg *config.Config, now time.Time) error {
	if !cfg.EventReadModelEnabled {
		return nil
	}
	window := eventReadModelWindow(cfg, now)
	if err := store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
		return fmt.Errorf("ensure calendar sync states: %w", err)
	}
	state, err := store.ClaimDueCalendarSync(ctx, "event-sync-"+uuid.NewString(), now, now.Add(workerLease))
	if err != nil || state == nil {
		return err
	}
	providers, err := factory.Build(ctx)
	if err != nil {
		// The claim must be released so a transient construction failure is not
		// held for a full lease. The worker never writes to providers here.
		finishErr := store.FailCalendarSync(ctx, *state, "provider_build_failed", time.Now().UTC(), time.Now().UTC().Add(time.Minute))
		return errors.Join(err, finishErr)
	}
	registry := calendar.NewRegistry(providers)
	service := eventsync.NewService(store, registry, eventSyncPolicyFor(cfg))
	return runClaimedEventSync(ctx, store, service, *state)
}

// eventReadModelWindow freezes the projection to UTC calendar days. Repeated
// worker ticks on one day therefore preserve the cursor; only a new UTC day
// rebases the rolling window and starts a replacement projection.
func eventReadModelWindow(cfg *config.Config, anchor time.Time) storage.SyncWindow {
	utc := anchor.UTC()
	day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return storage.SyncWindow{
		Start: day.AddDate(0, 0, -cfg.EventCacheLookbackDays),
		End:   day.AddDate(0, 0, cfg.EventCacheLookaheadDays),
	}
}

func eventSyncPolicyFor(cfg *config.Config) func(calendar.Provider) calendar.EventSyncPolicy {
	return func(provider calendar.Provider) calendar.EventSyncPolicy {
		// Start with adapter limits/retry behaviour and override only cadence.
		policy := calendar.EventSyncPolicy{}
		if configured, ok := calendar.EventSyncPolicyCapability(provider); ok {
			policy = configured.EventSyncPolicy()
		}
		switch provider.Name() {
		case "google":
			policy.PollInterval = cfg.EventSyncGoogleInterval
		case "microsoft":
			policy.PollInterval = cfg.EventSyncMicrosoftInterval
		case "apple":
			policy.PollInterval = cfg.EventSyncAppleInterval
		}
		return policy
	}
}

func runClaimedEventSync(ctx context.Context, store *storage.Store, service *eventsync.Service, state storage.CalendarSyncState) error {
	syncCtx, cancelSync := context.WithCancel(ctx)
	defer cancelSync()
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	ticker := time.NewTicker(workerHeartbeat)
	defer ticker.Stop()
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- maintainCalendarSyncLease(heartbeatCtx, ticker.C, func(at time.Time) error {
			return store.RenewCalendarSyncLease(heartbeatCtx, state, at.UTC(), at.UTC().Add(workerLease))
		}, cancelSync)
	}()
	err := service.RunOne(syncCtx, state)
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	return errors.Join(err, heartbeatErr)
}

func maintainCalendarSyncLease(ctx context.Context, ticks <-chan time.Time, renew func(time.Time) error, cancelWork context.CancelFunc) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok || ctx.Err() != nil {
				return nil
			}
			if err := renew(time.Now().UTC()); err != nil {
				if ctx.Err() != nil {
					// Work completion can race the final lease release; once the
					// heartbeat is stopped, that renewal result is not a failure.
					return nil //nolint:nilerr
				}
				cancelWork()
				return fmt.Errorf("renew calendar sync lease: %w", err)
			}
		}
	}
}

func executeJob(ctx context.Context, store *storage.Store, factory providerBuilder, job storage.Job, now time.Time) error {
	rule, err := store.RuleByID(ctx, job.RuleID)
	if err != nil {
		return err
	}
	run := storage.Run{ID: uuid.NewString(), JobID: job.ID, RuleID: rule.ID, Trigger: job.Kind, Outcome: "running", StartedAt: now, DryRun: job.Kind == "dry_run"}
	if err := store.StartRun(ctx, run); err != nil {
		return err
	}
	syncCtx, cancelSync := context.WithCancel(ctx)
	defer cancelSync()
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	ticker := time.NewTicker(workerHeartbeat)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- maintainJobLease(heartbeatCtx, ticker.C, func(at time.Time) (bool, error) {
			return store.RenewJobLease(heartbeatCtx, job, at.UTC())
		}, cancelSync)
	}()
	// Polling asks the active worker to stop but never claims a provider call
	// already dispatched has been revoked. Its reservation remains parked on
	// any ambiguous result.
	stopPollCtx, stopPoll := context.WithCancel(ctx)
	stopPollDone := make(chan struct{})
	go func() {
		defer close(stopPollDone)
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			requested, pollErr := store.JobStopRequested(stopPollCtx, job)
			if pollErr != nil || requested {
				if stopPollCtx.Err() == nil {
					cancelSync()
				}
				return
			}
			select {
			case <-stopPollCtx.Done():
				return
			case <-timer.C:
			}
		}
	}()
	var result syncengine.Result
	var reserved, dispatched bool
	operationID := uuid.NewString()
	if !run.DryRun {
		sum := sha256.Sum256([]byte("sync-run:" + run.ID))
		err = store.BeginExclusiveDispatch(syncCtx, storage.ExclusiveDispatchRequest{OperationID: operationID, TargetKey: storage.GlobalSyncDispatchKey, RequestHash: hex.EncodeToString(sum[:]), Kind: "run", CalendarIDs: []string{rule.TargetCalendarID}, Now: time.Now().UTC()})
		reserved = err == nil
		if err == nil {
			err = store.CheckSyncTargetEligibility(syncCtx, rule.ID)
		}
	}
	if err == nil && rule.SyncMode == "cached_instances" {
		if run.DryRun {
			var before, after int64
			before, err = store.RuleConfigVersion(syncCtx, rule.ID)
			if err == nil {
				rule, err = store.RuleByID(syncCtx, rule.ID)
			}
			if err == nil && (rule.SyncMode != "cached_instances" || rule.State != "paused") {
				err = storage.ErrRuleConfigChanged
			}
			if err == nil {
				result, err = syncengine.PreviewCacheCopy(syncCtx, store, rule, time.Now().UTC(), 15*time.Minute)
			}
			if err == nil {
				after, err = store.RuleConfigVersion(syncCtx, rule.ID)
			}
			if err == nil && before != after {
				err = storage.ErrRuleConfigChanged
			}
			if err == nil {
				result.Preview.RuleConfigVersion = before
			}
		} else {
			checkedAt := time.Now().UTC()
			_, err = store.GetSourceProjectionForCopy(syncCtx, rule.SourceCalendarID, checkedAt.AddDate(0, 0, -rule.LookbackDays), checkedAt.AddDate(0, 0, rule.LookaheadDays), checkedAt, 15*time.Minute)
			var providers []calendar.Provider
			if err == nil {
				providers, err = factory.Build(syncCtx)
			}
			if err == nil {
				engine := syncengine.New(calendar.NewRegistry(providers, calendar.RegistryOptions{Selection: store}), store).WithDispatchFence(newJobDispatchFence(store, job)).WithDispatchObserver(func() { dispatched = true })
				result, err = engine.RunCacheCopy(syncCtx, store, rule, time.Now().UTC(), 15*time.Minute)
			} else {
				err = &syncengine.Failure{Stage: "provider_setup", Cause: err}
			}
		}
	} else if err == nil {
		var providers []calendar.Provider
		providers, err = factory.Build(syncCtx)
		if err == nil {
			result, err = syncengine.New(calendar.NewRegistry(providers, calendar.RegistryOptions{Selection: store}), store).WithDispatchFence(newJobDispatchFence(store, job)).WithDispatchObserver(func() { dispatched = true }).Run(syncCtx, rule, run.DryRun)
		} else if !errors.Is(err, storage.ErrProjectionSnapshotUnavailable) {
			err = &syncengine.Failure{Stage: "provider_setup", Cause: err}
		}
	}
	stopPoll()
	<-stopPollDone
	stopHeartbeat()
	ticker.Stop()
	if heartbeatErr := <-heartbeatDone; heartbeatErr != nil {
		err = errors.Join(err, &syncengine.Failure{Stage: "job_lease", Cause: heartbeatErr})
	}
	// A successful engine run has committed all mapping changes. If any
	// provider call was attempted and the run failed, its outcome may be
	// unknown and the global reservation must remain parked indefinitely.
	if reserved && (err == nil || !dispatched) {
		if releaseErr := store.ReleaseExclusiveAfterConfirmedRun(ctx, operationID, []string{rule.TargetCalendarID}, time.Now().UTC()); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}
	finished := time.Now().UTC()
	if err != nil {
		code := syncengine.FailureCode(err)
		summary := syncengine.FailureMessage(code)
		finishErr := store.FinishRun(ctx, run.ID, job, "failed", finished, storage.Run{ErrorCode: code, ErrorSummary: summary, ProviderDispatched: dispatched})
		if finishErr != nil {
			return errors.Join(err, finishErr)
		}
		return err
	}
	summary := runWarningsSummary(result)
	if rule.SyncMode == "cached_instances" && run.DryRun {
		if summary != "" {
			summary += " "
		}
		summary += syncengine.SourceOnlyPreviewWarning
	}
	return store.FinishRun(ctx, run.ID, job, "succeeded", finished, storage.Run{
		CreatedCount: result.Created, UpdatedCount: result.Updated, DeletedCount: result.Deleted,
		SkippedCount: result.Skipped, WarningCount: result.Warnings,
		ErrorSummary: summary, Preview: result.Preview,
	})
}

// runWarningsSummary gives operators a safe explanation without event details.
func runWarningsSummary(result syncengine.Result) string {
	var messages []string
	if result.DetachedSeries > 0 {
		messages = append(messages, syncengine.DetachedSeriesWarning)
	}
	if result.ParticipantsIncomplete > 0 {
		messages = append(messages, syncengine.IncompleteParticipantsWarning)
	}
	return strings.Join(messages, " ")
}

func maintainJobLease(ctx context.Context, ticks <-chan time.Time, renew func(time.Time) (bool, error), cancelWork context.CancelFunc) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			owned, err := renew(time.Now().UTC())
			if err != nil {
				if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					return nil
				}
				cancelWork()
				return fmt.Errorf("renew worker lease: %w", err)
			}
			if !owned {
				cancelWork()
				return storage.ErrJobLeaseLost
			}
		}
	}
}
