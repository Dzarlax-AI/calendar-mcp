package runtime

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/eventsync"
	"calendar-mcp/internal/storage"
)

const (
	gatewayPublicationSlots = 3
	gatewayPublicationRetry = time.Minute
)

// The worker owns one queue of eligible sync generations. A slow provider
// occupies one slot while the others continue from the same round-robin cursor.
func runGatewayPublication(ctx context.Context, store *storage.Store, factory providerBuilder, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var running sync.WaitGroup
	defer running.Wait()

	type attempt struct {
		id  string
		err error
	}
	finished := make(chan attempt, gatewayPublicationSlots)
	active := make(map[string]bool, gatewayPublicationSlots)
	retryAt := make(map[string]time.Time)
	var after string
	for {
		now := time.Now()
		for id, at := range retryAt {
			if !now.Before(at) {
				delete(retryAt, id)
			}
		}
		if len(active) < gatewayPublicationSlots && ctx.Err() == nil {
			read, cancel := context.WithTimeout(ctx, interval)
			ids, err := store.GatewayPublicationCandidates(read, after, now)
			cancel()
			if err != nil {
				ids = nil
				if ctx.Err() == nil {
					log.Print("gateway event snapshot queue read failed")
				}
			}
			for _, id := range ids {
				if active[id] || now.Before(retryAt[id]) {
					continue
				}
				after = id
				active[id] = true
				running.Go(func() {
					call, cancel := context.WithTimeout(ctx, gatewayAuthorityTimeout)
					defer cancel()
					finished <- attempt{id, refreshGatewayPublication(call, store, factory, id)}
				})
				if len(active) == gatewayPublicationSlots {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case done := <-finished:
			delete(active, done.id)
			if done.err != nil {
				// A generation or authority change can become eligible again
				// on the next normal tick. Avoid immediately retrying it.
				delay := interval
				if !errors.Is(done.err, storage.ErrAuthorityChanged) {
					delay = gatewayPublicationRetry
					if ctx.Err() == nil {
						log.Print("gateway event snapshot refresh failed")
					}
				}
				retryAt[done.id] = time.Now().Add(delay)
			}
		}
	}
}

func refreshGatewayPublication(ctx context.Context, store *storage.Store, factory providerBuilder, id string) error {
	ticket, err := store.CaptureGatewayPublication(ctx, id, time.Now().UTC())
	if err != nil {
		return err
	}
	if strings.HasPrefix(id, "google:") {
		builder, ok := factory.(interface {
			BuildGatewayGooglePublication(context.Context, storage.GatewayPublicationTicket) (calendar.Provider, string, error)
		})
		if !ok {
			return errors.New("isolated Google publication provider unavailable")
		}
		provider, raw, err := builder.BuildGatewayGooglePublication(ctx, ticket)
		if err != nil {
			return err
		}
		return eventsync.RefreshGatewayPublication(ctx, store, provider, raw, ticket, time.Now)
	}
	providers, err := factory.Build(ctx)
	if err != nil {
		return err
	}
	registry := workerCalendarRegistry(providers, factory, nil)
	provider, raw, err := registry.ResolveContext(ctx, id)
	if err != nil {
		return err
	}
	return eventsync.RefreshGatewayPublication(ctx, store, provider, raw, ticket, time.Now)
}
