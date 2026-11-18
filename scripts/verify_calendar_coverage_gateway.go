//go:build ignore

// Local synthetic Calendar fixture for verify_calendar_coverage_gateway.py.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/mcpserver"
	"calendar-mcp/internal/storage"
)

// fakeLiveProvider rejects operations outside the fixture read path.
type fakeLiveProvider struct {
	name   string
	events map[string][]calendar.EventV2
}

func (p *fakeLiveProvider) Name() string { return p.name }
func (p *fakeLiveProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	out := make([]calendar.Calendar, 0, len(p.events))
	for id := range p.events {
		out = append(out, calendar.Calendar{ID: id})
	}
	return out, nil
}
func (p *fakeLiveProvider) ListEventsV2(_ context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	return calendar.Page[calendar.EventV2]{Items: slices.Clone(p.events[request.CalendarID]), Complete: true}, nil
}
func (p *fakeLiveProvider) GetEventV2(_ context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	for _, event := range p.events[ref.CalendarID] {
		if strings.TrimPrefix(event.ID, p.name+":") == ref.EventID {
			return &event, nil
		}
	}
	return nil, fmt.Errorf("synthetic event not found")
}

var errUnexpected = errors.New("unexpected synthetic provider operation")

func (p *fakeLiveProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, errUnexpected
}
func (p *fakeLiveProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	return nil, errUnexpected
}
func (p *fakeLiveProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	return nil, errUnexpected
}
func (p *fakeLiveProvider) DeleteEvent(context.Context, string, string) error { return errUnexpected }
func (p *fakeLiveProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{}, errUnexpected
}
func (p *fakeLiveProvider) CreateEventV2(context.Context, calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	return nil, errUnexpected
}
func (p *fakeLiveProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	return nil, errUnexpected
}
func (p *fakeLiveProvider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	return nil, errUnexpected
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	allReady := flag.Bool("all-ready", false, "leave all eight synthetic calendars available")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := os.MkdirTemp("", "calendar-coverage-fixture-")
	must(err)
	defer os.RemoveAll(dir)
	s, err := storage.Open(ctx, "sqlite://"+dir+"/fixture.db")
	must(err)
	defer s.Close()
	must(s.Migrate(ctx))
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Truncate(24 * time.Hour)
	window := storage.SyncWindow{Start: start.Add(-24 * time.Hour), End: start.Add(48 * time.Hour)}
	for _, provider := range []string{"google", "apple"} {
		id := provider + "-fixture"
		must(s.CreateConnection(ctx, storage.Connection{ID: id, Provider: provider, DisplayName: "synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
		n := 3
		if provider == "google" {
			n = 5
			snapshot, e := s.BeginAuthorization(ctx, id, nil, now)
			must(e)
			staged := make([]storage.DiscoveredDetailCalendar, n)
			for i := range staged {
				cid := fmt.Sprintf("google:%s:cal-%d", id, i)
				staged[i] = storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: cid, ConnectionID: id, ProviderCalendarID: fmt.Sprintf("cal-%d", i), Name: "synthetic", Timezone: "UTC", GooglePrimary: i == 0}, AccessRole: "owner"}
			}
			must(s.CompleteGoogleAuthorization(ctx, snapshot, staged, now))
		} else {
			for i := 0; i < n; i++ {
				cid := fmt.Sprintf("apple:%s:cal-%d", id, i)
				must(s.UpsertCalendar(ctx, storage.Calendar{ID: cid, ConnectionID: id, ProviderCalendarID: fmt.Sprintf("cal-%d", i), Name: "synthetic", Timezone: "UTC", CanRead: true, DiscoveredAt: now}))
			}
		}
	}
	must(s.EnsureCalendarSyncStates(ctx, now, window))
	live := map[string]*fakeLiveProvider{
		"google": {name: "google", events: map[string][]calendar.EventV2{}},
		"apple":  {name: "apple", events: map[string][]calendar.EventV2{}},
	}
	for i := 0; i < 8; i++ {
		state, e := s.ClaimDueCalendarSync(ctx, "seed", now, now.Add(time.Hour))
		must(e)
		if state == nil {
			panic("missing seed claim")
		}
		events := []storage.CachedEventUpsert{}
		for n := 0; n < 4; n++ {
			t := start.Add(time.Duration(i*4+n+1) * time.Minute)
			event := calendar.EventV2{ID: fmt.Sprintf("%s:event-%s-%d", strings.SplitN(state.CalendarID, ":", 2)[0], strings.ReplaceAll(state.CalendarID, ":", "-"), n), CalendarID: state.CalendarID, Provider: strings.SplitN(state.CalendarID, ":", 2)[0], Title: "synthetic", Status: "confirmed", InstanceKind: "single", Start: calendar.EventTime{DateTime: t.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: t.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
			event.Description = strings.Repeat("synthetic-description-", 200)
			event.Conference = &calendar.ConferenceData{EntryPoints: []calendar.ConferenceEntryPoint{{Type: "video", URI: "https://example.invalid/fixture-meeting"}}}
			events = append(events, storage.CachedEventUpsert{Event: event})
			if *allReady || state.CalendarID != "apple:apple-fixture:cal-2" {
				provider, raw, _ := strings.Cut(state.CalendarID, ":")
				live[provider].events[raw] = append(live[provider].events[raw], event)
			}
		}
		next := now.Add(time.Second)
		must(s.ApplyEventSyncPage(ctx, *state, storage.EventSyncBatch{FullSync: true, Upserts: events, NextSyncAt: &next}, true, now))
	}
	// Mark the chosen Apple calendar unavailable. The service test also covers
	// legacy parked rows that still exist in the cache.
	later := now.Add(2 * time.Second)
	for i := 0; i < 8; i++ {
		state, e := s.ClaimDueCalendarSync(ctx, "park", later, later.Add(time.Hour))
		must(e)
		if state == nil {
			panic("missing park claim")
		}
		if !*allReady && state.CalendarID == "apple:apple-fixture:cal-2" {
			must(s.ParkCalendarSync(ctx, *state, "permission", later))
		} else {
			next := later.Add(time.Hour)
			must(s.ApplyEventSyncPage(ctx, *state, storage.EventSyncBatch{NextSyncAt: &next}, true, later))
		}
	}
	cipher, e := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	must(e)
	reader, e := cachedpages.NewService(s, cipher, cachedpages.Options{Window: window})
	must(e)
	reg := calendar.NewRegistry([]calendar.Provider{live["google"], live["apple"]})
	mux := http.NewServeMux()
	must(mcpserver.RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "synthetic-upstream-key"}, mcpserver.Options{EnableV2: true, CatalogMode: mcpserver.CatalogModern, EventReadModelEnabled: true, CachedEvents: reader}))
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	must(e)
	json.NewEncoder(os.Stdout).Encode(map[string]any{"endpoint": "http://" + listener.Addr().String() + "/mcp", "start": start.Format(time.RFC3339), "end": start.Add(24 * time.Hour).Format(time.RFC3339)})
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		must(err)
	}
}
