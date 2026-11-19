//go:build ignore

// Synthetic loopback upstream for verify_gateway_cache_only.py.
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/eventsync"
	"calendar-mcp/internal/mcpserver"
	"calendar-mcp/internal/microsoft"
	"calendar-mcp/internal/runtime"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/token"
	"golang.org/x/oauth2"
)

type fixtureProvider struct {
	calendar.Provider
	calls        atomic.Int64
	events       []calendar.EventV2
	blocked      atomic.Bool
	account      string
	backend      string
	denial       atomic.Int64
	writes       atomic.Int64
	dispatchedAt atomic.Int64
	lookups      atomic.Int64
	offline      atomic.Bool
	failures     atomic.Int64
	receipts     sync.Map
	busyOnce     atomic.Bool
	busyStart    chan struct{}
	busyEnd      chan struct{}
}

func (p *fixtureProvider) Name() string      { return p.backend }
func (p *fixtureProvider) RouteName() string { return p.backend + "@" + p.account }
func (p *fixtureProvider) OwnsCalendar(id string) bool {
	return id == "fixture" || p.account == "affected" && id == "destination"
}
func (p *fixtureProvider) Capabilities(_ context.Context, _ string) (calendar.CalendarCapabilities, error) {
	p.calls.Add(1)
	return calendar.CalendarCapabilities{Operations: calendar.OperationCapabilities{Create: true}, MutationScopes: []calendar.MutationScope{calendar.ScopeSingle}, NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}, nil
}
func (p *fixtureProvider) CreateEventV2(_ context.Context, req calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	if req.Notifications != calendar.NotificationsNone || len(req.Event.Attendees) != 0 || req.Event.GatewayWriteID == "" {
		return nil, calendar.DefinitiveNoDispatch(errors.New("fixture forbids notification-bearing writes or missing marker"))
	}
	p.writes.Add(1)
	id := "acknowledged"
	if req.Event.Title == "lost response" {
		id = "recovered"
	}
	created := &calendar.EventV2{ID: id, Title: req.Event.Title, Provider: "apple", CalendarID: req.CalendarID,
		Visibility: "public", Start: req.Event.Start, End: req.Event.End}
	p.receipts.Store(req.Event.GatewayWriteID, created)
	if id == "recovered" {
		return nil, errors.New("synthetic response lost after dispatch")
	}
	return created, nil
}
func (p *fixtureProvider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	p.calls.Add(1)
	return nil, errors.New("unexpected live get")
}
func (*fixtureProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	return nil, calendar.DefinitiveNoDispatch(errors.New("fixture does not update events"))
}
func (p *fixtureProvider) DeleteEventV2(_ context.Context, req calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if req.Notifications != calendar.NotificationsNone || req.Ref.CalendarID != "fixture" || req.Ref.EventID != "instance" {
		return nil, calendar.DefinitiveNoDispatch(errors.New("unexpected fixture delete"))
	}
	p.writes.Add(1)
	p.dispatchedAt.Store(time.Now().UnixMilli())
	return nil, errors.New("synthetic delete response lost after dispatch")
}

func (p *fixtureProvider) MoveEventV2(_ context.Context, req calendar.MoveEventRequestV2) (*calendar.OperationResult, error) {
	if req.Notifications != calendar.NotificationsNone || req.Ref.CalendarID != "fixture" || req.Ref.EventID != "instance" || req.DestinationCalendarID != "destination" {
		return nil, calendar.DefinitiveNoDispatch(errors.New("unexpected fixture move"))
	}
	p.writes.Add(1)
	p.dispatchedAt.Store(time.Now().UnixMilli())
	return nil, errors.New("synthetic move response lost after dispatch")
}
func (p *fixtureProvider) FindGatewayWriteReceipt(_ context.Context, job storage.GatewayWrite) (*calendar.EventV2, error) {
	p.lookups.Add(1)
	event, _ := p.receipts.Load(job.ID)
	confirmed, _ := event.(*calendar.EventV2)
	return confirmed, nil
}
func (p *fixtureProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	p.calls.Add(1)
	return nil, errors.New("unexpected live discovery")
}
func (p *fixtureProvider) ListEventsV2(_ context.Context, _ calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.calls.Add(1)
	if p.blocked.Load() || p.offline.Load() {
		return calendar.Page[calendar.EventV2]{}, errors.New("synthetic provider offline")
	}
	return calendar.Page[calendar.EventV2]{Items: p.events, Complete: true}, nil
}

func (p *fixtureProvider) SyncEvents(ctx context.Context, _ calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	p.calls.Add(1)
	if p.busyStart != nil && p.busyOnce.CompareAndSwap(false, true) {
		close(p.busyStart)
		select {
		case <-p.busyEnd:
		case <-ctx.Done():
			return calendar.EventSyncPage{}, ctx.Err()
		}
	}
	if denied := int(p.denial.Load()); denied != 0 {
		class := calendar.EventSyncAuth
		if denied == http.StatusForbidden {
			class = calendar.EventSyncPermission
		}
		p.failures.Add(1)
		return calendar.EventSyncPage{}, &calendar.EventSyncError{Class: class, ProviderStatus: denied}
	}
	if p.offline.Load() {
		p.failures.Add(1)
		return calendar.EventSyncPage{}, &calendar.EventSyncError{Class: calendar.EventSyncTransient, ProviderStatus: http.StatusServiceUnavailable}
	}
	page := calendar.EventSyncPage{Complete: true}
	if p.Name() == "microsoft" {
		page.NextCursor = "synthetic-graph-cursor"
	}
	return page, nil
}

type fixtureResolver func(string) (calendar.Provider, string, error)

func (resolve fixtureResolver) Resolve(id string) (calendar.Provider, string, error) {
	return resolve(id)
}

const fixtureDAVStart = `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`
const fixtureDAVEnd = `</D:multistatus>`

type authorityTransport struct {
	ics         string
	familyReads atomic.Int64
	graphPages  atomic.Int64
	unexpected  atomic.Int64
}

func (f *authorityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	entry := func(href, props string) string {
		return `<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` + props +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
	}
	response := func(status int, mediaType, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header: http.Header{"Content-Type": {mediaType}, "ETag": {`"fixture"`}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	dav := func(entries string) (*http.Response, error) {
		return response(207, "application/xml", fixtureDAVStart+entries+fixtureDAVEnd)
	}
	if r.URL.Host == "graph.microsoft.com" && r.Method == "GET" && r.URL.Path == "/v1.0/me/calendars" {
		page := f.graphPages.Add(1)
		if page == 1 && r.URL.Query().Get("page") == "" {
			return response(200, "application/json", `{"value":[{"id":"private","name":"Private","canViewPrivateItems":false}],"@odata.nextLink":"https://graph.microsoft.com/v1.0/me/calendars?page=2"}`)
		}
		if page == 2 && r.URL.Query().Get("page") == "2" {
			return response(503, "application/json", `{"error":{"code":"ServiceUnavailable"}}`)
		}
	}
	if r.URL.Host == "caldav.fixture.invalid" {
		raw := r.URL.Path
		if r.Method == "PROPFIND" {
			switch raw {
			case "/":
				return dav(entry("/", `<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>`))
			case "/principal/":
				return dav(entry(raw, `<C:calendar-home-set><D:href>/home/</D:href></C:calendar-home-set>`))
			case "/home/":
				const resource = `<D:resourcetype><C:calendar/></D:resourcetype>`
				return dav(entry("/home/family/", resource+`<D:displayname>Family</D:displayname>`) +
					entry("/home/sibling/", resource+`<D:displayname>Sibling</D:displayname>`) +
					`<D:response><D:href>/home/blocked/</D:href><D:status>HTTP/1.1 503 Service Unavailable</D:status></D:response>`)
			case "/home/family/", "/home/sibling/":
				if r.Header.Get("Depth") == "1" && raw == "/home/family/" {
					return dav(entry(raw+"event%40fixture.ics", `<D:getetag>"fixture"</D:getetag>`))
				}
				if r.Header.Get("Depth") == "0" {
					return dav(entry(raw, `<D:owner><D:href>/delegate/</D:href></D:owner><D:current-user-privilege-set><D:privilege><D:read/></D:privilege></D:current-user-privilege-set>`))
				}
			}
		}
		if r.Method == "REPORT" {
			if raw == "/home/family/" {
				return response(500, "application/xml", fixtureDAVStart+"<D:response>")
			}
			if raw == "/home/sibling/" {
				return dav("")
			}
		}
		if r.Method == "GET" && r.URL.EscapedPath() == "/home/family/event%40fixture.ics" {
			f.familyReads.Add(1)
			return response(200, "text/calendar", f.ics)
		}
	}
	f.unexpected.Add(1)
	return nil, fmt.Errorf("unexpected synthetic authority request: %s %s", r.Method, r.URL.Path)
}

func publishAdapterEvidence(ctx context.Context, db *storage.Store, adapter calendar.CalendarReadEvidenceProvider,
	account, provider string, now time.Time) error {
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, now, time.Minute)
	require(err)
	calendars, err := db.ListCalendars(ctx, account)
	require(err)
	receipts := map[string]string{}
	for _, c := range calendars {
		receipts[c.ID] = c.DetailReceipt
	}
	ctx = calendar.WithGatewayReadRestriction(ctx, func(ctx context.Context, raw string, denied bool) error {
		id := calendar.AccountCalendarID(provider, account, raw)
		return db.RestrictGatewayCalendarRead(ctx, lease, raw, receipts[id], denied, now)
	})
	evidence, err := adapter.CheckCalendarReadEvidence(ctx)
	if err != nil {
		require(db.FailGatewayAuthorityCheck(ctx, lease, nil, now))
		return err
	}
	proofs := make([]storage.GatewayCalendarProof, 0, len(evidence))
	for _, item := range evidence {
		id := calendar.AccountCalendarID(provider, account, item.Calendar.ID)
		item.Calendar.ID, item.Calendar.Provider = id, provider
		item.Capabilities.CalendarID, item.Capabilities.Provider = id, provider
		class := storage.GatewayMetadataOnly
		if item.ReadPrivate {
			class = storage.GatewayPrivateAccess
		} else if item.ReadEvents {
			class = storage.GatewayEventAccess
		}
		proofs = append(proofs, storage.GatewayCalendarProof{Metadata: item.Calendar, Capabilities: item.Capabilities,
			Class: class, Unknown: item.Unknown, DetailReceipt: receipts[id]})
	}
	return db.PublishGatewayAuthority(ctx, lease, proofs, now)
}

func require(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tmp, err := os.MkdirTemp("", "strict-gateway-fixture-")
	require(err)
	defer os.RemoveAll(tmp)
	db, err := storage.Open(ctx, "sqlite://"+tmp+"/fixture.db")
	require(err)
	defer db.Close()
	require(db.Migrate(ctx))
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	require(err)
	db.SetArtifactCipher(cipher)
	now := time.Now().UTC().Truncate(time.Second)
	day := now.Truncate(24 * time.Hour)
	window := storage.SyncWindow{Start: day.AddDate(0, 0, -1), End: day.AddDate(0, 0, 2)}
	var clock atomic.Int64
	setTime := func(v time.Time) { clock.Store(v.UnixMilli()) }
	getTime := func() time.Time { return time.UnixMilli(clock.Load()).UTC() }
	setTime(now)
	var ids []string
	providers := map[string]*fixtureProvider{}
	proofs := map[string]storage.GatewayCalendarProof{}
	type fixtureCalendar struct{ name, account, raw string }
	fixtures := []fixtureCalendar{
		{"affected", "affected", "fixture"},
		{"healthy", "healthy", "fixture"},
	}
	var scenario string
	if len(os.Args) > 1 {
		scenario = os.Args[1]
	}
	pendingWrite := scenario == "delete_event" || scenario == "move_event"
	moving := scenario == "move_event"
	appliedWrite := pendingWrite && len(os.Args) > 2 && os.Args[2] == "--applied-write"
	graphRecovery := scenario == "graph-recovery"
	authorityAdapters := scenario == "authority-adapters"
	var authorityHTTP *authorityTransport
	var appleAdapter *apple.Provider
	var graphAdapter *microsoft.Provider
	if authorityAdapters {
		fixtures[0].raw = "/home/blocked/"
		fixtures = append(fixtures, fixtureCalendar{"family", "affected", "/home/family/"}, fixtureCalendar{"sibling", "affected", "/home/sibling/"}, fixtureCalendar{"graph-private", "graph", "private"})
		stamp := day.Add(10 * time.Hour).Format("20060102T150405Z")
		authorityHTTP = &authorityTransport{ics: "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Synthetic//Family fallback//EN\r\nBEGIN:VEVENT\r\nUID:fixture-family\r\nDTSTAMP:" + stamp +
			"\r\nDTSTART:" + stamp + "\r\nDURATION:PT1H\r\nSUMMARY:Real fallback fixture\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"}
		previous := http.DefaultTransport
		http.DefaultTransport = authorityHTTP
		defer func() { http.DefaultTransport = previous }()
		appleAdapter, err = apple.New("fixture", "fixture", "https://caldav.fixture.invalid")
		require(err)
		graphAdapter, err = microsoft.NewWithTokenStore("fixture", "fixture", "fixture", token.NewFileStore(tmp, "graph"),
			&oauth2.Token{AccessToken: "synthetic", Expiry: time.Now().Add(time.Hour)})
		require(err)
	}
	if pendingWrite || graphRecovery {
		fixtures = append(fixtures, fixtureCalendar{"destination", "affected", "destination"}, fixtureCalendar{"sibling", "affected", "sibling"})
	}
	accountProofs := map[string][]storage.GatewayCalendarProof{}
	for i, fixture := range fixtures {
		name, account, raw := fixture.name, fixture.account, fixture.raw
		backend := "apple"
		if graphRecovery && account == "affected" || account == "graph" {
			backend = "microsoft"
		}
		id := calendar.AccountCalendarID(backend, account, raw)
		ids = append(ids, id)
		if len(accountProofs[account]) == 0 {
			require(db.CreateConnection(ctx, storage.Connection{ID: account, Provider: backend, AccountFingerprint: account, Status: "connected",
				EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
		}
		require(db.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: account, ProviderCalendarID: raw, Name: name, Timezone: "UTC", CanRead: true, DiscoveredAt: now}))
		calendars, err := db.ListCalendars(ctx, account)
		require(err)
		proof := storage.GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: backend, Name: name, ReadOnly: true},
			Capabilities: calendar.CalendarCapabilities{Provider: backend, CalendarID: id, ReadOnly: true, Operations: calendar.OperationCapabilities{List: true, Get: true, Instances: true, Search: true}},
			Class:        storage.GatewayPrivateAccess}
		for _, entry := range calendars {
			if entry.ID == id {
				proof.DetailReceipt = entry.DetailReceipt
			}
		}
		if authorityAdapters && (name == "sibling" || name == "family") {
			proof.Class = storage.GatewayEventAccess
		}
		proofs[name] = proof
		accountProofs[account] = append(accountProofs[account], proof)
		start := day.Add(time.Duration(i+1) * time.Hour)
		event := calendar.EventV2{ID: "instance", RecurringEventID: "master", InstanceKind: "occurrence", Title: "needle " + name, Description: "synthetic", Visibility: "public",
			Start: calendar.EventTime{DateTime: start.Format(time.RFC3339), TimeZone: "UTC"},
			End:   calendar.EventTime{DateTime: start.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
		if name == "graph-private" {
			event.Visibility = "private"
		}
		master := event
		master.ID, master.RecurringEventID, master.InstanceKind = "master", "", "seriesMaster"
		master.Recurrence = []string{"RRULE:FREQ=DAILY"}
		cancelled := event
		cancelled.ID, cancelled.Status = "cancelled-instance", "cancelled"
		providers[name] = &fixtureProvider{account: account, backend: backend, events: []calendar.EventV2{event, master, cancelled}}
		if authorityAdapters && name == "healthy" {
			oversized := event
			oversized.ID, oversized.RecurringEventID, oversized.InstanceKind = "oversized-unrelated", "", "single"
			oversized.Title, oversized.Description = "Unrelated attachment", strings.Repeat("x", 96<<10)
			providers[name].events = append(providers[name].events, oversized)
		}
	}
	if authorityAdapters {
		// Simulate an Apple account carried forward from before migration 29.
		legacy, err := sql.Open("sqlite", tmp+"/fixture.db")
		require(err)
		_, err = legacy.ExecContext(ctx, `UPDATE connections SET authorization_revision='' WHERE id='affected' AND provider='apple'`)
		require(err)
		require(legacy.Close())
	}
	checkAuthority := func(account string, at time.Time) {
		lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, at, time.Minute)
		require(err)
		calendars, err := db.ListCalendars(ctx, account)
		require(err)
		results := slices.Clone(accountProofs[account])
		for i := range results {
			proof := &results[i]
			for _, entry := range calendars {
				if entry.ID == proof.Metadata.ID {
					proof.DetailReceipt = entry.DetailReceipt
					break
				}
			}
			// Fixtures emulate the split between a complete inventory and a
			// single temporary calendarView or CalDAV permission-probe failure.
			proof.Unknown = providers[proof.Metadata.Name].offline.Load()
		}
		require(db.PublishGatewayAuthority(ctx, lease, results, at.Add(time.Second)))
	}
	affectedProofAt := now.Add(-14 * time.Minute)
	if pendingWrite || graphRecovery || authorityAdapters {
		affectedProofAt = now.Add(-time.Minute)
	}
	checkAuthority("affected", affectedProofAt)
	checkAuthority("healthy", now.Add(-time.Minute))
	if authorityAdapters {
		checkAuthority("graph", now.Add(-time.Minute))
		binding, err := db.CredentialSnapshot(ctx, "affected")
		require(err)
		appleAdapter.SetCredentialBinding(binding.AuthorizationRevision, [32]byte{})
	}
	if pendingWrite {
		providers["healthy"].busyStart = make(chan struct{})
		providers["healthy"].busyEnd = make(chan struct{})
	}
	require(db.EnsureCalendarSyncStates(ctx, now, window))
	for range ids {
		state, err := db.ClaimDueCalendarSync(ctx, "synthetic", now, now.Add(time.Minute))
		require(err)
		next := now.Add(5 * time.Minute)
		if pendingWrite && state.CalendarID == ids[1] {
			next = now.Add(4 * time.Minute)
		}
		require(db.ApplyEventSyncPage(ctx, *state, storage.EventSyncBatch{FullSync: true, NextSyncAt: &next}, true, now))
	}
	refresh := func(name, raw string) {
		provider := providers[name]
		provider.blocked.Store(false)
		var reader calendar.Provider = provider
		if authorityAdapters && name == "family" && getTime().After(now) {
			reader = appleAdapter
		}
		ticket, err := db.CaptureGatewayPublication(ctx, proofs[name].Metadata.ID, getTime())
		if errors.Is(err, storage.ErrAuthorityChanged) {
			return
		}
		require(err)
		require(eventsync.RefreshGatewayPublication(ctx, db, reader, raw, ticket, getTime))
		provider.blocked.Store(true)
	}
	for _, fixture := range fixtures {
		refresh(fixture.name, fixture.raw)
	}
	byID := make(map[string]fixtureCalendar, len(fixtures))
	for _, fixture := range fixtures {
		byID[proofs[fixture.name].Metadata.ID] = fixture
	}
	syncer := eventsync.NewService(db, fixtureResolver(func(id string) (calendar.Provider, string, error) {
		fixture, ok := byID[id]
		if !ok {
			return nil, "", errors.New("unknown fixture calendar")
		}
		return providers[fixture.name], fixture.raw, nil
	}), nil)
	syncer.Now = getTime
	runSync := func() {
		for range fixtures {
			state, err := db.ClaimDueCalendarSync(ctx, "synthetic-recovery", getTime(), getTime().Add(5*time.Minute))
			require(err)
			if state == nil {
				return
			}
			fixture := byID[state.CalendarID]
			err = syncer.RunOne(ctx, *state)
			provider := providers[fixture.name]
			if (provider.offline.Load() || provider.denial.Load() != 0) && errors.Is(err, eventsync.ErrProviderFailure) {
				continue
			}
			require(err)
			refresh(fixture.name, fixture.raw)
		}
	}
	touched := []string{"affected"}
	if moving {
		touched = append(touched, "destination")
	}
	tick := func(at time.Time, accountOffline bool) {
		setTime(at)
		checkAt := at.Add(-time.Second)
		accounts, err := db.DueGatewayAuthorityChecks(ctx, checkAt, len(accountProofs))
		require(err)
		for _, account := range accounts {
			if accountOffline && account == "affected" {
				lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, checkAt, time.Minute)
				require(err)
				require(db.FailGatewayAuthorityCheck(ctx, lease, nil, at))
			} else {
				checkAuthority(account, checkAt)
			}
		}
		runSync()
	}
	svc, err := cachedpages.NewService(db, cipher, cachedpages.Options{Now: getTime, Window: window})
	require(err)
	reg := calendar.NewRegistry([]calendar.Provider{providers["affected"]}, calendar.RegistryOptions{Selection: db, WriteGuard: db})
	reg.SetProviderRefresh(func(_ context.Context, p calendar.Provider) (calendar.Provider, error) {
		providers["affected"].calls.Add(1)
		return p, nil
	})
	mux := http.NewServeMux()
	app := application.New(reg)
	authOptions := auth.Options{APIKey: "synthetic-upstream-key"}
	require(mcpserver.Register(mux, app, authOptions, svc))
	var busyDone chan struct{}
	var busy atomic.Bool
	if pendingWrite {
		recoveryCtx, cancelRecovery := context.WithCancel(ctx)
		recoveryDone := make(chan struct{})
		go func() {
			defer close(recoveryDone)
			runtime.RunGatewayWriteRecovery(recoveryCtx, db, providers["affected"], 5*time.Millisecond, getTime)
		}()
		defer func() {
			cancelRecovery()
			<-recoveryDone
		}()
	}
	var mu sync.Mutex
	var lease storage.GatewayAuthorityLease
	phase := "initial"
	mux.HandleFunc("/fixture/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			after := r.URL.Query().Get("phase")
			switch {
			case phase == "initial" && after == "grace":
				at := now.Add(50 * time.Second)
				lease, err = db.ClaimGatewayAuthorityCheck(ctx, "affected", at, 55*time.Second)
				require(err)
				setTime(now.Add(80 * time.Second))
			case phase == "grace" && after == "expired":
				require(db.FailGatewayAuthorityCheck(ctx, lease, nil, getTime()))
			case phase == "expired" && after == "renewed":
				at := now.Add(3 * time.Minute)
				current, err := db.ClaimGatewayAuthorityCheck(ctx, "affected", at, time.Minute)
				require(err)
				require(db.PublishGatewayAuthority(ctx, current, []storage.GatewayCalendarProof{proofs["affected"]}, at.Add(time.Second)))
				setTime(at.Add(time.Second))
				refresh("affected", "fixture")
			case phase == "renewed" && after == "recover-write":
				setTime(now.Add(4 * time.Minute))
				require(eventsync.ReconcileGatewayWrite(ctx, db, providers["affected"], getTime()))
				require(eventsync.ReconcileGatewayWrite(ctx, db, providers["affected"], getTime().Add(time.Minute)))
			case authorityAdapters && ((phase == "initial" && after == "apple-fallback") || (phase == "apple-fallback" && after == "apple-expired")):
				at := now.Add(6 * time.Minute)
				if after == "apple-expired" {
					at = now.Add(16 * time.Minute)
				}
				setTime(at)
				require(publishAdapterEvidence(ctx, db, appleAdapter, "affected", "apple", at))
				checkAuthority("healthy", at)
				checkAuthority("graph", at)
				runSync()
			case authorityAdapters && phase == "apple-expired" && after == "graph-page-error":
				setTime(now.Add(22 * time.Minute))
				var api *calendar.APIError
				err := publishAdapterEvidence(ctx, db, graphAdapter, "graph", "microsoft", getTime())
				if !errors.As(err, &api) || authorityHTTP.graphPages.Load() != 2 {
					require(fmt.Errorf("expected Graph failure on its second page: %w", err))
				}
			case graphRecovery && phase == "initial" && after == "graph-denied":
				providers["affected"].denial.Store(http.StatusUnauthorized)
				providers["destination"].denial.Store(http.StatusForbidden)
				setTime(now.Add(5 * time.Minute))
				runSync()
			case graphRecovery && phase == "graph-denied" && after == "graph-recovered":
				for _, name := range []string{"affected", "destination"} {
					providers[name].denial.Store(0)
					for i := range providers[name].events {
						providers[name].events[i].Title += " fresh"
					}
				}
				tick(now.Add(6*time.Minute), false)
			case pendingWrite && phase == "initial" && after == "before-timeout":
				setTime(now.Add(4 * time.Minute))
				busyDone = make(chan struct{})
				go func() {
					busy.Store(true)
					defer close(busyDone)
					defer busy.Store(false)
					runSync()
				}()
				select {
				case <-providers["healthy"].busyStart:
				case <-time.After(2 * time.Second):
					require(errors.New("normal sync did not enter blocked provider"))
				}
			case pendingWrite && phase == "before-timeout" && after == "after-timeout":
				setTime(time.UnixMilli(providers["affected"].dispatchedAt.Load()).Add(5 * time.Minute))
				// The recovery goroutine must release the reservation while
				// the normal event-sync goroutine is still inside SyncEvents.
				for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
					claim, err := db.CaptureGatewayPublication(ctx, ids[0], getTime())
					if claim.Grant.Metadata.ID != "" || !errors.Is(err, storage.ErrAuthorityChanged) {
						require(errors.New("old publication became valid after timeout"))
					}
					read, err := db.ReadGatewayEvents(ctx, storage.GatewayEventQuery{CalendarRef: ids[0], EventID: "instance"}, getTime(), false)
					require(err)
					if len(read.Sources) == 1 && read.Sources[0].Source.ErrorCode == "" {
						break
					}
					if time.Now().After(deadline) {
						require(errors.New("write reservation stayed pending while normal sync was busy"))
					}
				}
				if !busy.Load() {
					require(errors.New("normal sync completed before timeout verification"))
				}
			case pendingWrite && phase == "after-timeout" && after == "offline-1h":
				if appliedWrite {
					source := providers["affected"]
					if moving {
						event := source.events[0]
						event.ID, event.RecurringEventID, event.InstanceKind = "moved-instance", "", "single"
						providers["destination"].events = append(providers["destination"].events, event)
					}
					source.events = slices.DeleteFunc(source.events, func(event calendar.EventV2) bool { return event.ID == "instance" })
				}
				for _, name := range touched {
					providers[name].offline.Store(true)
				}
				close(providers["healthy"].busyEnd)
				<-busyDone
				for minute := 7; minute <= 60; minute++ {
					tick(now.Add(time.Duration(minute)*time.Minute), false)
				}
			case pendingWrite && phase == "offline-1h" && after == "offline-24h":
				// The production polling interval is five minutes. Advance at
				// that cadence for the rest of the synthetic day.
				for minute := 65; minute <= 24*60; minute += 5 {
					tick(now.Add(time.Duration(minute)*time.Minute), false)
				}
			case pendingWrite && phase == "offline-24h" && after == "account-offline":
				for _, fixture := range fixtures {
					if fixture.account == "affected" {
						providers[fixture.name].offline.Store(true)
					}
				}
				for minute := 24*60 + 1; minute <= 24*60+20; minute++ {
					tick(now.Add(time.Duration(minute)*time.Minute), true)
				}
			case pendingWrite && phase == "account-offline" && after == "resynced":
				for _, fixture := range fixtures {
					providers[fixture.name].offline.Store(false)
				}
				for _, name := range touched {
					for i := range providers[name].events {
						providers[name].events[i].Title += " fresh"
					}
				}
				back := getTime()
				for minute := 1; minute <= 6; minute++ {
					tick(back.Add(time.Duration(minute)*time.Minute), false)
				}
			default:
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			phase = after
		}
		var calls int64
		failures := map[string]int64{}
		for name, provider := range providers {
			calls += provider.calls.Load()
			failures[name] = provider.failures.Load()
		}
		state := map[string]any{"calls": calls, "phase": phase, "writes": providers["affected"].writes.Load(),
			"lookups": providers["affected"].lookups.Load(), "failures": failures, "busy": busy.Load()}
		if authorityAdapters {
			state["adapter_calls"] = []int64{authorityHTTP.familyReads.Load(), authorityHTTP.graphPages.Load(), authorityHTTP.unexpected.Load()}
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	info := map[string]any{"endpoint": server.URL + "/mcp", "control": server.URL + "/fixture/state",
		"start": day, "end": day.AddDate(0, 0, 1), "affected": ids[0], "healthy": ids[1]}
	if pendingWrite || graphRecovery {
		info["destination"] = ids[2]
		info["sibling"] = ids[3]
	}
	if authorityAdapters {
		info["family"], info["sibling"], info["graph_private"] = ids[2], ids[3], ids[4]
	}
	require(json.NewEncoder(os.Stdout).Encode(info))
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "fixture stopped")
}
