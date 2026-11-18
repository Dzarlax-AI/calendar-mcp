package runtime

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"calendar-mcp/internal/storage"
)

const (
	gatewayAuthorityWorkers = 4
	gatewayAuthorityTimeout = 45 * time.Second
	gatewayAuthorityTick    = 5 * time.Second
)

type gatewayAuthorityChecker interface {
	CheckGatewayAuthority(context.Context, storage.GatewayAuthorityLease) ([]storage.GatewayCalendarProof, error)
}

// runGatewayAuthority is independent of the event cache and rules queues.
// Each worker owns at most one provider request sequence, and the durable
// per-account check lease also protects against other worker processes.
func runGatewayAuthority(ctx context.Context, store *storage.Store, checker gatewayAuthorityChecker) {
	ticker := time.NewTicker(gatewayAuthorityTick)
	defer ticker.Stop()
	inflight := make(map[string]bool)
	done := make(chan string, gatewayAuthorityWorkers)
	var active sync.WaitGroup
	defer active.Wait()
	schedule := func() {
		if len(inflight) == gatewayAuthorityWorkers {
			return
		}
		ids, err := store.DueGatewayAuthorityChecks(ctx, time.Now(), gatewayAuthorityWorkers)
		if err != nil {
			log.Print("gateway calendar authorization scheduling failed")
			return
		}
		for _, id := range ids {
			if len(inflight) == gatewayAuthorityWorkers {
				break
			}
			if inflight[id] {
				continue
			}
			inflight[id] = true
			active.Go(func() {
				checkGatewayAccount(ctx, store, checker, id)
				done <- id
			})
		}
	}
	schedule()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-done:
			delete(inflight, id)
			schedule()
		case <-ticker.C:
			schedule()
		}
	}
}

func checkGatewayAccount(ctx context.Context, store *storage.Store, checker gatewayAuthorityChecker, id string) {
	lease, err := store.ClaimGatewayAuthorityCheck(ctx, id, time.Now(), gatewayAuthorityTimeout+10*time.Second)
	if err != nil {
		if !errors.Is(err, storage.ErrAuthorityChanged) && ctx.Err() == nil {
			log.Print("gateway calendar authorization claim failed")
		}
		return
	}
	call, cancel := context.WithTimeout(ctx, gatewayAuthorityTimeout)
	proofs, checkErr := checker.CheckGatewayAuthority(call, lease)
	if checkErr == nil && proofs == nil {
		// A non-nil completed inventory can include unknown source probes.
		// Keep the positive evidence obtained before the account deadline.
		checkErr = call.Err()
	}
	cancel()
	finish, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if checkErr == nil && ctx.Err() == nil {
		err = store.PublishGatewayAuthority(finish, lease, proofs, time.Now())
		if err == nil {
			return
		}
		if !errors.Is(err, storage.ErrAuthorityChanged) {
			log.Print("gateway calendar authorization publication failed")
		}
	}
	var denial *storage.GatewayAuthorityDenial
	_ = errors.As(checkErr, &denial)
	if err := store.FailGatewayAuthorityCheck(finish, lease, denial, time.Now()); err != nil && !errors.Is(err, storage.ErrAuthorityChanged) {
		log.Print("gateway calendar authorization claim release failed")
	}
}
