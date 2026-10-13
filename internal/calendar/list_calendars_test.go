package calendar

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type listingProvider struct {
	Provider
	name string
	read func(context.Context) ([]Calendar, error)
}

func (p *listingProvider) Name() string                                        { return p.name }
func (p *listingProvider) ListCalendars(c context.Context) ([]Calendar, error) { return p.read(c) }
func TestListCalendarsBoundedConcurrentOrderAndCopies(t *testing.T) {
	entered := make(chan int, 8)
	release := make(chan struct{})
	var active, max atomic.Int32
	raw := make([][]Calendar, 8)
	ps := make([]Provider, 8)
	calls := make([]atomic.Int32, 8)
	for i := range ps {
		raw[i] = []Calendar{{ID: fmt.Sprint(i)}}
		ps[i] = &listingProvider{name: fmt.Sprintf("p%d", i), read: func(ctx context.Context) ([]Calendar, error) {
			calls[i].Add(1)
			n := active.Add(1)
			for old := max.Load(); n > old; old = max.Load() {
				if max.CompareAndSwap(old, n) {
					break
				}
			}
			defer active.Add(-1)
			entered <- i
			select {
			case <-release:
				return raw[i], nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	}
	r := NewRegistry(ps)
	done := make(chan []Calendar, 1)
	go func() {
		v, e := r.ListCalendars(t.Context())
		if e != nil {
			t.Error(e)
		}
		done <- v
	}()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("four workers did not start")
		}
	}
	if max.Load() != 4 {
		t.Fatal("worker limit", max.Load())
	}
	close(release)
	v := <-done
	if len(v) != 8 {
		t.Fatal(len(v))
	}
	for i, c := range v {
		if c.ID != fmt.Sprintf("p%d:%d", i, i) || c.Provider != fmt.Sprintf("p%d", i) || raw[i][0].ID != fmt.Sprint(i) || calls[i].Load() != 1 {
			t.Fatalf("order/copy/call invariant at %d", i)
		}
	}
	if active.Load() != 0 || max.Load() > 4 {
		t.Fatal("unjoined or excess workers")
	}
}
func TestListCalendarsDeterministicErrorsNoPartial(t *testing.T) {
	first := errors.New("first failure")
	second := errors.New("second failure")
	blocked := make(chan struct{})
	entered := make(chan struct{})
	r := NewRegistry([]Provider{&listingProvider{name: "first", read: func(context.Context) ([]Calendar, error) { <-blocked; return nil, first }}, &listingProvider{name: "second", read: func(context.Context) ([]Calendar, error) { close(entered); return nil, second }}})
	done := make(chan error, 1)
	go func() {
		v, e := r.ListCalendars(t.Context())
		if v != nil {
			t.Error("partial result")
		}
		done <- e
	}()
	<-entered
	close(blocked)
	err := <-done
	if !errors.Is(err, first) || err.Error() != "first: first failure" {
		t.Fatal(err)
	}
}
func TestListCalendarsCancellationAndReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{}, 8)
	var calls atomic.Int32
	ps := make([]Provider, 8)
	for i := range ps {
		ps[i] = &listingProvider{name: "old", read: func(c context.Context) ([]Calendar, error) {
			calls.Add(1)
			entered <- struct{}{}
			<-c.Done()
			return nil, c.Err()
		}}
	}
	r := NewRegistry(ps)
	done := make(chan error, 1)
	go func() { _, e := r.ListCalendars(ctx); done <- e }()
	for range 4 {
		<-entered
	}
	r.ReplaceProviders([]Provider{&listingProvider{name: "new", read: func(context.Context) ([]Calendar, error) { return []Calendar{{ID: "fresh"}}, nil }}})
	v, e := r.ListCalendars(t.Context())
	if e != nil || len(v) != 1 || v[0].ID != "new:fresh" {
		t.Fatalf("replacement %#v %v", v, e)
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("workers not joined")
	}
	if calls.Load() != 4 {
		t.Fatal("queued providers were called", calls.Load())
	}
}
func TestListCalendarsConcurrentRequestsBoundedPerRequest(t *testing.T) {
	var total, active, peak atomic.Int32
	var wg sync.WaitGroup
	entered := make(chan struct{}, 96)
	release := make(chan struct{})
	ps := make([]Provider, 12)
	for i := range ps {
		ps[i] = &listingProvider{name: "p", read: func(context.Context) ([]Calendar, error) {
			total.Add(1)
			n := active.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			defer active.Add(-1)
			entered <- struct{}{}
			<-release
			return []Calendar{{ID: "same"}}, nil
		}}
	}
	r := NewRegistry(ps)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := r.ListCalendars(t.Context())
			if e != nil || len(v) != 12 {
				t.Errorf("load result %d %v", len(v), e)
			}
		}()
	}
	for range 32 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("workers did not start")
		}
	}
	if peak.Load() != 32 {
		t.Error("unexpected per-request load limit", peak.Load())
	}
	close(release)
	wg.Wait()
	if total.Load() != 96 || active.Load() != 0 || peak.Load() > 32 {
		t.Fatalf("total=%d active=%d peak=%d", total.Load(), active.Load(), peak.Load())
	}
}
