package apple

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type discoveryFixture struct {
	mu         sync.Mutex
	counts     map[string]int
	home       string
	count      int
	failPath   string
	failStatus int
	block      chan struct{}
	entered    chan struct{}
	server     *httptest.Server
}

func newDiscoveryFixture(t *testing.T) *discoveryFixture {
	f := &discoveryFixture{counts: map[string]int{}, home: "/home/", count: 1, entered: make(chan struct{}, 100)}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}
func (f *discoveryFixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.counts[r.URL.Path]++
	home, count, status, fail, block := f.home, f.count, f.failStatus, f.failPath, f.block
	f.mu.Unlock()
	if r.URL.Path == "/" {
		f.entered <- struct{}{}
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	if r.URL.Path == fail && status != 0 {
		w.WriteHeader(status)
	} else {
		w.WriteHeader(207)
	}
	fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
	response := func(href, props string) {
		fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href, props)
	}
	switch r.URL.Path {
	case "/":
		response("/", `<d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal>`)
	case "/principal/":
		response("/principal/", `<c:calendar-home-set><d:href>`+home+`</d:href></c:calendar-home-set>`)
	default:
		for i := 0; i < count; i++ {
			response(fmt.Sprintf("%sc%d/", r.URL.Path, i), `<d:resourcetype><c:calendar/></d:resourcetype><d:displayname>synthetic</d:displayname>`)
		}
	}
	fmt.Fprint(w, `</d:multistatus>`)
}
func (f *discoveryFixture) provider(t *testing.T) *Provider {
	p, e := New("synthetic", "synthetic", f.server.URL)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func (f *discoveryFixture) calls(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[path]
}
func TestDiscoveryTTLAndLiveCalendarList(t *testing.T) {
	f := newDiscoveryFixture(t)
	p := f.provider(t)
	var clock atomic.Int64
	p.discovery.now = func() time.Time { return time.Unix(0, clock.Load()) }
	if v, e := p.ListCalendars(t.Context()); e != nil || len(v) != 1 {
		t.Fatalf("cold %v %v", v, e)
	}
	clock.Store(int64(9 * time.Minute))
	f.mu.Lock()
	f.count = 2
	f.mu.Unlock()
	if v, e := p.ListCalendars(t.Context()); e != nil || len(v) != 2 {
		t.Fatalf("live %v %v", v, e)
	}
	clock.Store(int64(10 * time.Minute))
	if _, e := p.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	if f.calls("/") != 2 || f.calls("/principal/") != 2 || f.calls("/home/") != 3 {
		t.Fatal("TTL extended or list cached")
	}
}
func TestDiscoveryConcurrentColdRequestsCoalesce(t *testing.T) {
	f := newDiscoveryFixture(t)
	f.block = make(chan struct{})
	p := f.provider(t)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := p.ListCalendars(t.Context()); errs <- e }()
	}
	<-f.entered
	close(f.block)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if f.calls("/") != 1 || f.calls("/principal/") != 1 || f.calls("/home/") != 12 {
		t.Fatal("discovery not coalesced or list not live")
	}
}
func TestDiscoveryCanceledWaiterAndLeader(t *testing.T) {
	f := newDiscoveryFixture(t)
	f.block = make(chan struct{})
	p := f.provider(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, e := p.ListCalendars(ctx); done <- e }()
	<-f.entered
	waiter, stop := context.WithCancel(t.Context())
	stop()
	if _, e := p.ListCalendars(waiter); e != context.Canceled {
		t.Fatal(e)
	}
	cancel()
	if e := <-done; e == nil {
		t.Fatal("canceled discovery succeeded")
	}
	close(f.block)
	if _, e := p.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	if f.calls("/") != 2 || f.calls("/home/") != 1 {
		t.Fatal("canceled discovery cached")
	}
}
func TestDiscoveryInvalidStatusNoRetry(t *testing.T) {
	for _, status := range []int{401, 403, 404, 410} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newDiscoveryFixture(t)
			p := f.provider(t)
			if _, e := p.ListCalendars(t.Context()); e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			f.failPath = "/home/"
			f.failStatus = status
			f.home = "/moved/"
			f.mu.Unlock()
			if v, e := p.ListCalendars(t.Context()); e == nil || v != nil {
				t.Fatalf("normalized invalid status succeeded %v %v", v, e)
			}
			if f.calls("/") != 1 || f.calls("/home/") != 2 {
				t.Fatal("hidden retry")
			}
			f.mu.Lock()
			f.failStatus = 0
			f.mu.Unlock()
			if _, e := p.ListCalendars(t.Context()); e != nil {
				t.Fatal(e)
			}
			if f.calls("/") != 2 || f.calls("/moved/") != 1 {
				t.Fatal("next call did not rediscover")
			}
		})
	}
}
func TestDiscoveryErrorsIsolationAndGeneration(t *testing.T) {
	f := newDiscoveryFixture(t)
	p := f.provider(t)
	f.failPath = "/principal/"
	f.failStatus = 404
	if _, e := p.ListCalendars(t.Context()); e == nil {
		t.Fatal("failed discovery succeeded")
	}
	f.mu.Lock()
	f.failStatus = 0
	f.mu.Unlock()
	if _, e := p.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	other := f.provider(t)
	if _, e := other.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	if f.calls("/") != 3 {
		t.Fatal("provider instances share cache")
	}
	p.discovery.mu.Lock()
	generation := p.discovery.generation
	p.discovery.mu.Unlock()
	p.invalidateDiscovery(generation - 1)
	if _, e := p.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	if f.calls("/") != 3 {
		t.Fatal("late failure invalidated newer cache")
	}
}

// Signals when a follower reaches the discovery-flight wait without timing sleeps.
type flightWaitContext struct {
	context.Context
	entered chan<- struct{}
}

func (c flightWaitContext) Done() <-chan struct{} { c.entered <- struct{}{}; return c.Context.Done() }
func TestDiscoveryFailureSharedWithWaitingCallers(t *testing.T) {
	f := newDiscoveryFixture(t)
	f.block = make(chan struct{})
	f.failPath = "/principal/"
	f.failStatus = 404
	p := f.provider(t)
	errors := make(chan error, 12)
	go func() { _, _, e := p.calendarHomeSet(t.Context()); errors <- e }()
	<-f.entered
	waiting := make(chan struct{}, 11)
	for range 11 {
		go func() { _, _, e := p.calendarHomeSet(flightWaitContext{t.Context(), waiting}); errors <- e }()
	}
	for range 11 {
		<-waiting
	}
	close(f.block)
	for range 12 {
		if e := <-errors; e == nil {
			t.Fatal("failed discovery succeeded")
		}
	}
	if f.calls("/") != 1 || f.calls("/principal/") != 1 {
		t.Fatal("waiting calls retried discovery")
	}
	f.mu.Lock()
	f.failStatus = 0
	f.mu.Unlock()
	if _, e := p.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	if f.calls("/") != 2 {
		t.Fatal("error was cached")
	}
}
func TestDiscoveryWaiterCancellationDoesNotCancelLeader(t *testing.T) {
	f := newDiscoveryFixture(t)
	f.block = make(chan struct{})
	p := f.provider(t)
	leader := make(chan error, 1)
	go func() { _, _, e := p.calendarHomeSet(t.Context()); leader <- e }()
	<-f.entered
	ctx, cancel := context.WithCancel(t.Context())
	waiting := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { _, _, e := p.calendarHomeSet(flightWaitContext{ctx, waiting}); done <- e }()
	<-waiting
	cancel()
	if e := <-done; e != context.Canceled {
		t.Fatal(e)
	}
	close(f.block)
	if e := <-leader; e != nil {
		t.Fatal(e)
	}
	if _, e := p.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	if f.calls("/") != 1 {
		t.Fatal("waiter canceled leader cache")
	}
}

func TestDiscoveryCancellationAtCompletionNotPublished(t *testing.T) {
	f := newDiscoveryFixture(t)
	p := f.provider(t)
	ctx, cancel := context.WithCancel(t.Context())
	p.discovery.now = func() time.Time { cancel(); return time.Unix(0, 0) }
	if _, e := p.ListCalendars(ctx); e != context.Canceled {
		t.Fatal(e)
	}
	p.discovery.now = func() time.Time { return time.Unix(0, 0) }
	if _, e := p.ListCalendars(t.Context()); e != nil {
		t.Fatal(e)
	}
	if f.calls("/") != 2 {
		t.Fatal("canceled discovery was published")
	}
}
