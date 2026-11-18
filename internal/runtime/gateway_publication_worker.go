package runtime

import (
	"context"
	"errors"
	"log"
	"time"

	"calendar-mcp/internal/eventsync"
	"calendar-mcp/internal/storage"
)

// runGatewayPublication keeps optional provider listings off the rule-job and
// event-sync loop. Durable successful sync state is the input queue, so a
// restart does not lose work. The cursor only orders attempts for fairness.
func runGatewayPublication(ctx context.Context, store *storage.Store, factory providerBuilder, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var after string
	for {
		call, cancel := context.WithTimeout(ctx, gatewayAuthorityTimeout)
		err := refreshNextGatewayPublication(call, store, factory, &after)
		cancel()
		if err != nil && !errors.Is(err, storage.ErrAuthorityChanged) && ctx.Err() == nil {
			// Provider details and page tokens stay out of worker logs.
			log.Print("gateway event snapshot refresh failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func refreshNextGatewayPublication(ctx context.Context, store *storage.Store, factory providerBuilder, after *string) error {
	now := time.Now().UTC()
	id, err := store.NextGatewayPublication(ctx, *after, now)
	if err == nil && id == "" && *after != "" {
		*after = ""
		id, err = store.NextGatewayPublication(ctx, "", now)
	}
	if err != nil || id == "" {
		return err
	}
	*after = id
	providers, err := factory.Build(ctx)
	if err != nil {
		return err
	}
	registry := workerCalendarRegistry(providers, factory, nil)
	provider, raw, err := registry.ResolveContext(ctx, id)
	if err != nil {
		return err
	}
	return eventsync.RefreshGatewayPublication(ctx, store, provider, raw, id, time.Now)
}
