package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	providerfactory "calendar-mcp/internal/providers"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/syncengine"
)

var errWorkerNotConfigured = errors.New("worker storage is not configured")

const workerLease = 15 * time.Minute

type providerBuilder interface {
	Build(context.Context) ([]calendar.Provider, error)
}

func Worker(ctx context.Context) error {
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		return errWorkerNotConfigured
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
	healthListener, err := net.Listen("tcp", cfg.WorkerHealthAddr)
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
	if err := runWorkerCycle(waitCtx, store, factory, time.Now().UTC()); err != nil {
		log.Printf("calendar worker cycle failed: category=%T", err)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			if err := runWorkerCycle(waitCtx, store, factory, now.UTC()); err != nil {
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
	if _, err := store.RecoverStaleJobs(ctx, now.Add(-workerLease), now); err != nil {
		return err
	}
	if _, err := store.ScheduleDueJobs(ctx, now); err != nil {
		return err
	}
	workerID := "worker-" + uuid.NewString()
	for {
		job, err := store.ClaimJob(ctx, workerID, now)
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

func executeJob(ctx context.Context, store *storage.Store, factory providerBuilder, job storage.Job, now time.Time) error {
	rule, err := store.RuleByID(ctx, job.RuleID)
	if err != nil {
		return err
	}
	run := storage.Run{ID: uuid.NewString(), JobID: job.ID, RuleID: rule.ID, Trigger: job.Kind, Outcome: "running", StartedAt: now, DryRun: job.Kind == "dry_run"}
	if err := store.StartRun(ctx, run); err != nil {
		return err
	}
	providers, err := factory.Build(ctx)
	var result syncengine.Result
	if err == nil {
		result, err = syncengine.New(calendar.NewRegistry(providers), store).Run(ctx, rule, run.DryRun)
	}
	finished := time.Now().UTC()
	if err != nil {
		code, summary := "sync_failed", "Provider synchronization failed"
		var recurrenceErr *syncengine.RecurrenceCompatibilityError
		if errors.As(err, &recurrenceErr) {
			code, summary = "recurrence_unsupported", "Target calendar cannot preserve this recurrence pattern; the rule remains paused."
		}
		finishErr := store.FinishRun(ctx, run.ID, job.ID, "failed", finished, storage.Run{ErrorCode: code, ErrorSummary: summary})
		if finishErr != nil {
			return errors.Join(err, finishErr)
		}
		return err
	}
	return store.FinishRun(ctx, run.ID, job.ID, "succeeded", finished, storage.Run{
		CreatedCount: result.Created, UpdatedCount: result.Updated, DeletedCount: result.Deleted,
		SkippedCount: result.Skipped, WarningCount: result.Warnings,
	})
}
