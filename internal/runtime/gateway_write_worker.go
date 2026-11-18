package runtime

import (
	"context"
	"log"
	"sync"
	"time"

	"calendar-mcp/internal/eventsync"
	"calendar-mcp/internal/storage"
)

const gatewayWriteRecoveryTick = 5 * time.Second

func runGatewayWriteRecovery(ctx context.Context, store *storage.Store, lookup eventsync.GatewayWriteFinder) {
	RunGatewayWriteRecovery(ctx, store, lookup, gatewayWriteRecoveryTick, time.Now)
}

// RunGatewayWriteRecovery runs independent, bounded expiration and marker-read
// loops. interval and now allow isolated fixtures to exercise the scheduler.
func RunGatewayWriteRecovery(ctx context.Context, store *storage.Store, lookup eventsync.GatewayWriteFinder, interval time.Duration, now func() time.Time) {
	if interval <= 0 || now == nil {
		return
	}
	var loops sync.WaitGroup
	run := func(timeout time.Duration, step func(context.Context) error) {
		defer loops.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			call, cancel := context.WithTimeout(ctx, timeout)
			err := step(call)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Print("gateway write recovery remains pending")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
	loops.Add(2)
	go run(5*time.Second, func(call context.Context) error { return store.ExpireOneGatewayWrite(call, now()) })
	go run(15*time.Second, func(call context.Context) error { return eventsync.ReconcileGatewayWrite(call, store, lookup, now()) })
	loops.Wait()
}
