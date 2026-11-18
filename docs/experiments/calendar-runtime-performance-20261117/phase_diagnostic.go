//go:build ignore

// Isolated runtime/SDK acceptance harness. All fixtures and credentials are
// synthetic; the harness has no provider and never starts runtime.Serve.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/mcpserver"
	"calendar-mcp/internal/storage"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const syntheticKey = "synthetic-calendar-r6-sdk-key"

var anchor = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
var window = storage.SyncWindow{Start: anchor.Add(-24 * time.Hour), End: anchor.Add(48 * time.Hour)}

type options struct {
	Mode, DatabaseURL, Scenario                                      string
	Events, Sources, Concurrency, Walks, Rounds, PageSize, BodyBytes int
	Background                                                       bool
}

type sample struct {
	Phase                                                                              string `json:"phase"`
	Mode                                                                               string `json:"mode"`
	Round, Worker, Items, Pages, Markers, SDKBytes, MaxSDKBytes                        int
	FirstMS, WalkMS                                                                    float64
	Digest, TitlesDigest                                                               string
	ReferenceDigest                                                                    string
	JSONRPCFrameBytes, HTTPResponseBodyBytes, HTTPWireReceivedBytes, HTTPWireSentBytes int64
}

type wireCounts struct{ received, sent, body, frame atomic.Int64 }

func (c *wireCounts) reset() { c.received.Store(0); c.sent.Store(0); c.body.Store(0); c.frame.Store(0) }

type wireConn struct {
	net.Conn
	counts *wireCounts
}

func (c *wireConn) Read(p []byte) (int, error) {
	n, e := c.Conn.Read(p)
	c.counts.received.Add(int64(n))
	return n, e
}
func (c *wireConn) Write(p []byte) (int, error) {
	n, e := c.Conn.Write(p)
	c.counts.sent.Add(int64(n))
	return n, e
}

type countedResponseBody struct {
	io.ReadCloser
	counts *wireCounts
	sse    bool
	line   int
	data   bool
}

func (b *countedResponseBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.counts.body.Add(int64(n))
	if !b.sse {
		b.counts.frame.Add(int64(n))
		return n, e
	}
	var frame int64
	for _, value := range p[:n] {
		if value == '\n' {
			b.line = 0
			b.data = true
			continue
		}
		if b.line < 6 {
			if value != "data: "[b.line] {
				b.data = false
			}
		} else if b.data && value != '\r' {
			frame++
		}
		b.line++
	}
	b.counts.frame.Add(frame)
	return n, e
}

type countingTransport struct {
	base   http.RoundTripper
	counts *wireCounts
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != "127.0.0.1" && r.URL.Hostname() != "::1" {
		return nil, errors.New("non-loopback SDK request blocked")
	}
	response, err := t.base.RoundTrip(r)
	if response != nil && response.Body != nil {
		response.Body = &countedResponseBody{ReadCloser: response.Body, counts: t.counts, sse: strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream"), data: true}
	}
	return response, err
}

type memory struct {
	HeapAlloc, HeapInuse, TotalAlloc, Sys, SelfMaxRSSBytes uint64
	NumGC                                                  uint32
}

type phaseSample struct {
	Read              cachedpages.PageStats `json:"service"`
	SDKResultRenderNS int64                 `json:"sdk_result_render_ns"`
	SDKResultBytes    int                   `json:"sdk_result_bytes"`
}

type measuredReader struct {
	*cachedpages.Service
	capture bool
	mu      sync.Mutex
	phases  []phaseSample
}

func (r *measuredReader) ObserveCachedResult(stats cachedpages.PageStats, elapsed time.Duration, bytes int) {
	if !r.capture {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases = append(r.phases, phaseSample{stats, elapsed.Nanoseconds(), bytes})
}

func checkpoint() memory {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	rss := uint64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	return memory{m.HeapAlloc, m.HeapInuse, m.TotalAlloc, m.Sys, rss, m.NumGC}
}

func main() {
	o := options{}
	flag.StringVar(&o.Mode, "mode", "candidate", "seed, candidate, baseline, or speed")
	flag.StringVar(&o.DatabaseURL, "database-url", "", "explicit disposable database URL")
	flag.StringVar(&o.Scenario, "scenario", "ordinary", "ordinary, wide, dense, sparse, jumbo, or jumbo-master")
	flag.IntVar(&o.Events, "events", 400, "number of seeded events")
	flag.IntVar(&o.Sources, "sources", 1, "number of Google sources (1..16)")
	flag.IntVar(&o.Concurrency, "concurrency", 1, "concurrent SDK clients (1 or 4)")
	flag.IntVar(&o.Walks, "walks", 3, "complete walks per memory worker")
	flag.IntVar(&o.Rounds, "rounds", 20, "alternating paired warm speed rounds")
	flag.IntVar(&o.PageSize, "page-size", 25, "upper bound, 1..50")
	flag.IntVar(&o.BodyBytes, "body-bytes", 2048, "unrelated description bytes per event")
	flag.BoolVar(&o.Background, "background", false, "same-pool cached UI reads and real sync-page writes")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(o options) error {
	if o.Events < 1 || o.Events > 10000 || o.Sources < 1 || o.Sources > 16 || (o.Concurrency != 1 && o.Concurrency != 4) || o.PageSize < 1 || o.PageSize > 50 || o.Walks < 1 || o.Rounds < 1 || o.BodyBytes < 0 || o.BodyBytes > 1<<20 {
		return errors.New("invalid synthetic fixture limits")
	}
	if o.Mode != "seed" && o.Mode != "candidate" && o.Mode != "baseline" && o.Mode != "speed" {
		return errors.New("unknown mode")
	}
	if err := validateDisposableURL(o.DatabaseURL); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store, err := storage.Open(ctx, o.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if o.Mode == "seed" {
		return seed(ctx, store, o)
	}
	if err = store.CheckSchema(ctx); err != nil {
		return err
	}
	before := checkpoint()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		return err
	}
	reader, err := cachedpages.NewService(store, cipher, cachedpages.Options{Window: window})
	if err != nil {
		return err
	}
	measured := &measuredReader{Service: reader, capture: true}
	ids := calendarIDs(o.Sources)
	request := cachedpages.Request{CalendarIDs: ids, Start: anchor, End: anchor.Add(24 * time.Hour), PageSize: o.PageSize}
	reg := calendar.NewRegistry(nil)
	mux := http.NewServeMux()
	if err = mcpserver.RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: syntheticKey}, mcpserver.Options{EnableV2: true, CatalogMode: mcpserver.CatalogModern, EventReadModelEnabled: true, CachedEvents: measured}); err != nil {
		return err
	}
	// The baseline is the existing full-result Store API through a harness-only
	// SDK tool. It is explicitly not a production MCP contract being replaced.
	baseline := server.NewMCPServer("synthetic-full-cache-baseline", "1")
	baseline.AddTool(mcp.NewTool("full_cached_baseline"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		events, _, err := store.ListCachedEvents(ctx, ids, request.Start, request.End)
		if err != nil {
			return mcp.NewToolResultError("baseline cache unavailable"), nil
		}
		data, err := json.Marshal(events)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultStructured(events, string(data)), nil
	})
	mux.Handle("/baseline", auth.Middleware(auth.Options{APIKey: syntheticKey})(server.NewStreamableHTTPServer(baseline)))
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	stopBackground := func() {}
	var backgroundReads, backgroundWrites atomic.Int64
	if o.Background {
		stopBackground = background(ctx, store, ids, &backgroundReads, &backgroundWrites)
	}
	defer stopBackground()
	rows := []sample{}
	var snapshots = []memory{before}
	runGroup := func(mode string, round int, firstOnly bool) error {
		var group sync.WaitGroup
		var mu sync.Mutex
		errorsCh := make(chan error, o.Concurrency)
		for worker := 0; worker < o.Concurrency; worker++ {
			group.Add(1)
			go func(worker int) {
				defer group.Done()
				row, err := walk(ctx, httpServer.URL, mode, request, round, worker, firstOnly)
				if err != nil {
					errorsCh <- err
					return
				}
				expected := o.Events
				if o.Scenario == "sparse" {
					expected = min(25, expected)
				}
				if o.Scenario == "jumbo-master" {
					expected--
				}
				if (!firstOnly || mode == "baseline") && row.Items != expected {
					errorsCh <- fmt.Errorf("fixture traversal cardinality: got %d expected %d (%s)", row.Items, expected, mode)
					return
				}
				mu.Lock()
				rows = append(rows, row)
				mu.Unlock()
			}(worker)
		}
		group.Wait()
		close(errorsCh)
		for err := range errorsCh {
			return err
		}
		return nil
	}
	if o.Mode == "speed" {
		// Warm both paths once; retain no page payloads across calls.
		for _, mode := range []string{"baseline", "candidate"} {
			if err = runGroup(mode, -1, true); err != nil {
				return err
			}
		}
		rows = rows[:0]
		measured.mu.Lock()
		measured.phases = nil
		measured.mu.Unlock()
		for round := 0; round < o.Rounds; round++ {
			modes := []string{"baseline", "candidate"}
			if round%2 == 1 {
				modes[0], modes[1] = modes[1], modes[0]
			}
			for _, mode := range modes {
				if err = runGroup(mode, round, true); err != nil {
					return err
				}
			}
		}
		for round := 0; round < 3; round++ {
			modes := []string{"baseline", "candidate"}
			if round%2 == 1 {
				modes[0], modes[1] = modes[1], modes[0]
			}
			for _, mode := range modes {
				if err = runGroup(mode, round, false); err != nil {
					return err
				}
			}
		}
	} else {
		for round := 0; round < o.Walks; round++ {
			if err = runGroup(o.Mode, round, false); err != nil {
				return err
			}
			snapshots = append(snapshots, checkpoint())
		}
	}
	stopBackground()
	// Group rows contain only bounded counters and two digests, never pages or
	// per-event signatures. Main memory runs never invoke the opposite path.
	stats := map[string]any{}
	for _, mode := range []string{"baseline", "candidate"} {
		first, full := []float64{}, []float64{}
		for _, row := range rows {
			if row.Mode == mode {
				if row.Phase == "first_page" || o.Mode != "speed" {
					first = append(first, row.FirstMS)
				}
				if row.Phase == "full_walk" {
					full = append(full, row.WalkMS)
				}
			}
		}
		if len(first) > 0 {
			stats[mode] = map[string]any{"first_p50_ms": percentile(first, .5), "first_p95_ms": percentile(first, .95), "walk_p50_ms": percentile(full, .5), "walk_max_ms": percentile(full, 1), "first_page_samples": len(first), "full_walk_samples": len(full)}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic": true, "options": o, "samples": rows, "latency": stats, "memory": snapshots, "page_phases": measured.phases, "background_reads": backgroundReads.Load(), "background_sync_page_writes": backgroundWrites.Load(), "provider_calls": 0, "pool_tuning": false, "scope": "actual Store + cachedpages + auth + SDK client/server in one loopback process; baseline is harness-only full-cache Store wrapper; result-render timing is exact text/structured SDK object construction, client wall includes HTTP/envelope serialization and decode"})
}

func validateDisposableURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "sqlite" {
		p := strings.TrimPrefix(raw, "sqlite://")
		absolute, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(absolute, "/private/tmp/calendar-r6-") && !strings.HasPrefix(absolute, "/tmp/calendar-r6-") {
			return errors.New("SQLite fixture must be an explicit calendar-r6 path in tmp")
		}
		return nil
	}
	if (u.Scheme == "postgres" || u.Scheme == "postgresql") && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") && strings.HasPrefix(u.Path, "/calendar_r6_") && u.User.Username() == "calendar_r6" {
		return nil
	}
	return errors.New("only explicit disposable synthetic loopback PostgreSQL or tmp SQLite is allowed")
}

func calendarIDs(count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("google:r6-fixture:calendar-%02d", i)
	}
	return ids
}

func seed(ctx context.Context, store *storage.Store, o options) error {
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	if err := store.CreateConnection(ctx, storage.Connection{ID: "r6-fixture", Provider: "google", DisplayName: "Synthetic runtime fixture", Status: "connected", EncryptedCredentials: []byte("synthetic-unused-no-provider"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: anchor, UpdatedAt: anchor}); err != nil {
		return err
	}
	snapshot, err := store.BeginAuthorization(ctx, "r6-fixture", nil, anchor)
	if err != nil {
		return err
	}
	ids := calendarIDs(o.Sources)
	staged := make([]storage.DiscoveredDetailCalendar, len(ids))
	for i, id := range ids {
		staged[i] = storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: id, ConnectionID: "r6-fixture", ProviderCalendarID: fmt.Sprintf("calendar-%02d", i), Name: "Synthetic calendar", GooglePrimary: i == 0}, AccessRole: "owner"}
	}
	if err = store.CompleteGoogleAuthorization(ctx, snapshot, staged, anchor); err != nil {
		return err
	}
	if err = store.EnsureCalendarSyncStates(ctx, anchor, window); err != nil {
		return err
	}
	seenSources := map[string]bool{}
	seededRows := 0
	for i := 0; i < o.Sources; i++ {
		state, err := store.ClaimDueCalendarSync(ctx, "seed", anchor, anchor.Add(time.Hour))
		if err != nil {
			return err
		}
		if state == nil {
			return errors.New("fixture sync claim unavailable")
		}
		if seenSources[state.CalendarID] {
			return errors.New("fixture source was claimed twice before all sources were seeded")
		}
		seenSources[state.CalendarID] = true
		nextSeedSync := anchor.Add(time.Hour)
		batch := storage.EventSyncBatch{FullSync: true, NextSyncAt: &nextSeedSync}
		for n := 0; n < o.Events; n++ {
			if ids[n%len(ids)] != state.CalendarID {
				continue
			}
			start := anchor.Add(time.Duration(n+1) * time.Second)
			if o.Scenario == "sparse" && n >= 25 {
				start = anchor.Add(25*time.Hour + time.Duration(n)*time.Second)
			}
			title := fmt.Sprintf("Useful synthetic title %06d", n)
			kind := "single"
			if (o.Scenario == "jumbo" || o.Scenario == "jumbo-master") && n == 0 {
				title = strings.Repeat("wide-title-", 4000)
				if o.Scenario == "jumbo-master" {
					kind = "seriesMaster"
				}
			}
			event := calendar.EventV2{ID: fmt.Sprintf("event-%06d", n), CalendarID: state.CalendarID, Provider: "google", Title: title, Description: strings.Repeat("d", o.BodyBytes), Status: "confirmed", InstanceKind: kind, Start: calendar.EventTime{DateTime: start.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: start.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
			batch.Upserts = append(batch.Upserts, storage.CachedEventUpsert{Event: event})
			seededRows++
		}
		if err = store.ApplyEventSyncPage(ctx, *state, batch, true, anchor); err != nil {
			return err
		}
	}
	if seededRows != o.Events || len(seenSources) != o.Sources {
		return errors.New("fixture seed cardinality mismatch")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"seeded_events": seededRows, "seeded_sources": len(seenSources), "sources": o.Sources, "scenario": o.Scenario, "schema": storage.SchemaVersion, "synthetic": true})
}

func walk(ctx context.Context, base, mode string, request cachedpages.Request, round, worker int, firstOnly bool) (sample, error) {
	phase := "full_walk"
	if firstOnly {
		phase = "first_page"
	}
	row := sample{Mode: mode, Phase: phase, Round: round, Worker: worker}
	path := "/mcp/modern"
	if mode == "baseline" {
		path = "/baseline"
	}
	counts := &wireCounts{}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &wireConn{Conn: connection, counts: counts}, nil
	}
	defer httpTransport.CloseIdleConnections()
	c, err := client.NewStreamableHttpClient(base+path, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + syntheticKey}), transport.WithHTTPBasicClient(&http.Client{Transport: countingTransport{base: httpTransport, counts: counts}}))
	if err != nil {
		return row, err
	}
	defer c.Close()
	if err = c.Start(ctx); err != nil {
		return row, err
	}
	initialize := mcp.InitializeRequest{}
	initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcp.Implementation{Name: "synthetic-runtime-check", Version: "1"}
	if _, err = c.Initialize(ctx, initialize); err != nil {
		return row, err
	}
	counts.reset()
	started := time.Now()
	digest, titles, references := sha256.New(), sha256.New(), sha256.New()
	token := ""
	for {
		call := mcp.CallToolRequest{}
		call.Params.Name = "get_cached_events"
		call.Params.Arguments = map[string]any{"calendar_ids": request.CalendarIDs, "start": request.Start.Format(time.RFC3339), "end": request.End.Format(time.RFC3339), "page_size": request.PageSize, "page_token": token}
		if mode == "baseline" {
			call.Params.Name = "full_cached_baseline"
			call.Params.Arguments = map[string]any{}
		}
		result, err := c.CallTool(ctx, call)
		if err != nil {
			return row, err
		}
		if result.IsError {
			return row, fmt.Errorf("%s tool rejected synthetic request: %+v", mode, result.Content)
		}
		if row.Pages == 0 {
			row.FirstMS = float64(time.Since(started).Nanoseconds()) / 1e6
		}
		wire, err := json.Marshal(result)
		if err != nil {
			return row, err
		}
		row.SDKBytes += len(wire)
		row.MaxSDKBytes = max(row.MaxSDKBytes, len(wire))
		row.Pages++
		if mode == "candidate" {
			if len(wire) > cachedpages.MaxResultBytes {
				return row, errors.New("SDK result exceeded 16 KiB")
			}
			var page cachedpages.Page
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				return row, err
			}
			if err = json.Unmarshal(data, &page); err != nil {
				return row, err
			}
			for _, item := range page.Items {
				row.Items++
				if item.Kind != "event" {
					row.Markers++
				}
				hashSummary(digest, titles, item.EventID, item.CalendarID, item.Title, item.Start, item.End, item.Status)
				hashReference(references, item.EventID, item.CalendarID, item.Start, item.End)
			}
			if firstOnly {
				break
			}
			if page.NextPageToken == "" {
				if !page.Complete {
					return row, errors.New("terminal page not complete")
				}
				break
			}
			if page.Complete || len(page.Items) == 0 || page.NextPageToken == token {
				return row, errors.New("invalid continuation progress")
			}
			token = page.NextPageToken
		} else {
			var events []calendar.EventV2
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				return row, err
			}
			if err = json.Unmarshal(data, &events); err != nil {
				return row, err
			}
			for _, event := range events {
				row.Items++
				hashSummary(digest, titles, event.ID, event.CalendarID, event.Title, event.Start, event.End, event.Status)
				hashReference(references, event.ID, event.CalendarID, event.Start, event.End)
			}
			break
		}
		if row.Pages > 10000 {
			return row, errors.New("walk did not terminate")
		}
	}
	row.WalkMS = float64(time.Since(started).Nanoseconds()) / 1e6
	row.Digest = hex.EncodeToString(digest.Sum(nil))
	row.TitlesDigest = hex.EncodeToString(titles.Sum(nil))
	row.ReferenceDigest = hex.EncodeToString(references.Sum(nil))
	row.JSONRPCFrameBytes = counts.frame.Load()
	row.HTTPResponseBodyBytes = counts.body.Load()
	row.HTTPWireReceivedBytes = counts.received.Load()
	row.HTTPWireSentBytes = counts.sent.Load()
	return row, nil
}

func hashSummary(digest, titles hash.Hash, id, cid, title string, start, end calendar.EventTime, status string) {
	data, _ := json.Marshal([]any{id, cid, start, end, status})
	_, _ = digest.Write(data)
	data, _ = json.Marshal(title)
	_, _ = titles.Write(data)
}

func hashReference(digest hash.Hash, id, cid string, start, end calendar.EventTime) {
	canonical := func(t calendar.EventTime) string {
		if t.Date != "" {
			return t.Date
		}
		parsed, err := time.Parse(time.RFC3339Nano, t.DateTime)
		if err != nil {
			return "invalid"
		}
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	data, _ := json.Marshal([]string{id, cid, canonical(start), canonical(end)})
	_, _ = digest.Write(data)
}
func percentile(values []float64, p float64) float64 {
	sort.Float64s(values)
	i := int(float64(len(values))*p+.999999) - 1
	if i < 0 {
		i = 0
	}
	return values[i]
}

func background(parent context.Context, store *storage.Store, ids []string, reads, writes *atomic.Int64) func() {
	ctx, cancel := context.WithCancel(parent)
	var once sync.Once
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, _, err := store.ListCachedEvents(ctx, ids, anchor, anchor.Add(time.Hour)); err == nil {
					reads.Add(1)
				}
				state, err := store.ClaimDueCalendarSync(ctx, "synthetic-background", time.Now().UTC(), time.Now().UTC().Add(time.Hour))
				if err == nil && state != nil {
					if err = store.ApplyEventSyncPage(ctx, *state, storage.EventSyncBatch{NextCursor: "synthetic-background"}, true, time.Now().UTC()); err == nil {
						writes.Add(1)
					}
				}
			}
		}
	}()
	return func() { once.Do(func() { cancel(); done.Wait() }) }
}
