package apple

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const discoveryTTL = 10 * time.Minute

type discoveryStatusKey struct{}
type discoveryStatus struct{ code atomic.Int32 }
type discoveryFlight struct {
	done       chan struct{}
	homeSet    string
	generation uint64
	err        error
}

type discoveryCache struct {
	mu         sync.Mutex
	flight     *discoveryFlight
	homeSet    string
	expires    time.Time
	generation uint64
	now        func() time.Time // Injected only by local tests.
}

func (c *discoveryCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (p *Provider) calendarHomeSet(ctx context.Context) (string, uint64, error) {
	c := &p.discovery
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		c.mu.Lock()
		if c.homeSet != "" && c.clock().Before(c.expires) {
			home, generation := c.homeSet, c.generation
			c.mu.Unlock()
			return home, generation, nil
		}
		if c.flight != nil {
			flight := c.flight
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", 0, ctx.Err()
			case <-flight.done:
				if err := ctx.Err(); err != nil {
					return "", 0, err
				}
				return flight.homeSet, flight.generation, flight.err
			}
		}
		c.flight = &discoveryFlight{done: make(chan struct{})}
		c.mu.Unlock()
		status := &discoveryStatus{}
		discoveryCtx := context.WithValue(ctx, discoveryStatusKey{}, status)
		principal, err := p.client.FindCurrentUserPrincipal(discoveryCtx)
		if err == nil && status.code.Load() != 0 {
			err = fmt.Errorf("CalDAV discovery HTTP %d", status.code.Load())
		}
		if err == nil && principal == "" {
			err = fmt.Errorf("find principal: empty path")
		}
		home := ""
		if err == nil {
			home, err = p.client.FindCalendarHomeSet(discoveryCtx, principal)
			if err != nil {
				err = fmt.Errorf("find home set: %w", err)
			}
			if err == nil && status.code.Load() != 0 {
				err = fmt.Errorf("CalDAV discovery HTTP %d", status.code.Load())
			}
			if err == nil && home == "" {
				err = fmt.Errorf("find home set: empty path")
			}
		} else {
			err = fmt.Errorf("find principal: %w", err)
		}
		finished := time.Time{}
		if err == nil {
			finished = c.clock()
			err = ctx.Err()
		}
		c.mu.Lock()
		if err == nil {
			err = ctx.Err()
		}
		c.generation++
		if err == nil {
			c.homeSet = home
			c.expires = finished.Add(discoveryTTL)
		} else {
			c.homeSet = ""
			c.expires = time.Time{}
		}
		generation := c.generation
		c.flight.homeSet, c.flight.generation, c.flight.err = home, generation, err
		close(c.flight.done)
		c.flight = nil
		c.mu.Unlock()
		return home, generation, err
	}
}
func (p *Provider) invalidateDiscovery(generation uint64) {
	c := &p.discovery
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation == generation {
		c.homeSet = ""
		c.expires = time.Time{}
		c.generation++
	}
}
