package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func syntheticGuardStore(t *testing.T) *storage.Store {
	t.Helper()
	databaseURL := "sqlite://" + filepath.Join(t.TempDir(), "fence.db")
	if raw := os.Getenv("CALENDAR_SYNTHETIC_POSTGRES_URL"); raw != "" {
		admin, err := sql.Open("pgx", raw)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("write_fence_%d", time.Now().UnixNano())
		if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
			_ = admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = admin.Close()
		})
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		databaseURL = u.String()
	}
	s, err := storage.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = s.CreateConnection(t.Context(), storage.Connection{ID: "synthetic", Provider: "google", AccountFingerprint: "synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"google:source", "google:target"} {
		if err = s.UpsertCalendar(t.Context(), storage.Calendar{ID: id, ConnectionID: "synthetic", ProviderCalendarID: id[len("google:"):], Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestAllTypedWritesRespectSharedReservation(t *testing.T) {
	s := syntheticGuardStore(t)
	p := newGuardTestProvider()
	app := New(calendar.NewRegistry([]calendar.Provider{p}, calendar.RegistryOptions{WriteGuard: s}))
	calls := []struct {
		name string
		call func() error
	}{
		{"create", func() error {
			_, e := app.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return e
		}},
		{"update", func() error {
			_, e := app.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return e
		}},
		{"delete", func() error {
			_, e := app.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return e
		}},
		{"respond", func() error {
			_, e := app.RespondToEvent(t.Context(), calendar.RespondToEventRequestV2{Ref: guardRef(), Response: "accepted"})
			return e
		}},
		{"move", func() error {
			_, e := app.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: guardRef(), DestinationCalendarID: "google:target"})
			return e
		}},
		{"import", func() error {
			_, e := app.ImportEvent(t.Context(), calendar.ImportEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return e
		}},
	}
	release, err := s.BeginWriteDispatch(t.Context(), []string{"google:source"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range calls {
		t.Run(tc.name+"-blocked", func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, calendar.ErrWriteDispatchBlocked) || p.writes != 0 {
				t.Fatalf("err=%v writes=%d", err, p.writes)
			}
		})
	}
	if err := release(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i, tc := range calls {
		t.Run(tc.name+"-allowed", func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if p.writes != i+1 {
				t.Fatalf("writes=%d", p.writes)
			}
		})
	}
	p.fail = context.DeadlineExceeded
	if err := calls[0].call(); !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) {
		t.Fatalf("ambiguous error: %v", err)
	}
	if err := calls[1].call(); !errors.Is(err, calendar.ErrWriteDispatchBlocked) {
		t.Fatalf("uncertain result did not retain fence: %v", err)
	}
}

type blockingWriteProvider struct {
	*guardTestProvider
	entered chan struct{}
	resume  chan struct{}
}

func (p *blockingWriteProvider) CreateEventV2(ctx context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	close(p.entered)
	<-p.resume
	return p.guardTestProvider.CreateEventV2(ctx, request)
}
func TestConcurrentProviderWriteHoldsReservation(t *testing.T) {
	store := syntheticGuardStore(t)
	p := &blockingWriteProvider{guardTestProvider: newGuardTestProvider(), entered: make(chan struct{}), resume: make(chan struct{})}
	app := New(calendar.NewRegistry([]calendar.Provider{p}, calendar.RegistryOptions{WriteGuard: store}))
	done := make(chan error, 1)
	go func() {
		_, err := app.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
		done <- err
	}()
	<-p.entered
	_, err := app.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
	if !errors.Is(err, calendar.ErrWriteDispatchBlocked) {
		t.Fatalf("concurrent delete not blocked: %v", err)
	}
	close(p.resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = app.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIFenceAndSyncFenceExcludeEachOther(t *testing.T) {
	store := syntheticGuardStore(t)
	ids := []string{"google:source"}
	release, err := store.BeginWriteDispatch(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	req := storage.ExclusiveDispatchRequest{OperationID: "synthetic-sync-dispatch-0001", TargetKey: storage.GlobalSyncDispatchKey, RequestHash: "0000000000000000000000000000000000000000000000000000000000000000", Kind: "run", CalendarIDs: ids, Now: time.Now().UTC()}
	if err = store.BeginExclusiveDispatch(t.Context(), req); !errors.Is(err, storage.ErrExclusiveDispatchConflict) {
		t.Fatalf("sync crossed API fence: %v", err)
	}
	if err = release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = store.BeginExclusiveDispatch(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginWriteDispatch(t.Context(), ids); !errors.Is(err, storage.ErrExclusiveDispatchConflict) {
		t.Fatalf("API crossed sync fence: %v", err)
	}
	if err = store.ReleaseExclusiveAfterConfirmedRun(t.Context(), req.OperationID, ids, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

type legacyFenceProvider struct{ *guardTestProvider }

func (p *legacyFenceProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	p.writes++
	return &calendar.Event{ID: "synthetic"}, nil
}
func (p *legacyFenceProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	p.writes++
	return &calendar.Event{ID: "synthetic"}, nil
}
func (p *legacyFenceProvider) DeleteEvent(context.Context, string, string) error {
	p.writes++
	return nil
}
func TestLegacyWritesRespectSharedReservation(t *testing.T) {
	store := syntheticGuardStore(t)
	p := &legacyFenceProvider{newGuardTestProvider()}
	reg := calendar.NewRegistry([]calendar.Provider{p}, calendar.RegistryOptions{WriteGuard: store})
	calls := []func() error{
		func() error {
			_, err := reg.CreateEvent(t.Context(), "google:source", calendar.EventCreate{})
			return err
		},
		func() error {
			_, err := reg.UpdateEvent(t.Context(), "google:source", "synthetic", calendar.EventUpdate{})
			return err
		},
		func() error { return reg.DeleteEvent(t.Context(), "google:source", "synthetic") },
	}
	release, err := store.BeginWriteDispatch(t.Context(), []string{"google:source"})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if err := call(); !errors.Is(err, calendar.ErrWriteDispatchBlocked) {
			t.Fatalf("legacy write crossed fence: %v", err)
		}
	}
	if p.writes != 0 {
		t.Fatalf("blocked calls reached provider: %d", p.writes)
	}
	if err = release(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if err = call(); err != nil {
			t.Fatal(err)
		}
	}
	if p.writes != len(calls) {
		t.Fatalf("allowed writes: %d", p.writes)
	}
}

type blockingReadModel struct {
	*stubEventReadModel
	entered, resume chan struct{}
}

func (m *blockingReadModel) UpsertCachedEvent(ctx context.Context, event calendar.EventV2, at time.Time) error {
	close(m.entered)
	<-m.resume
	return m.stubEventReadModel.UpsertCachedEvent(ctx, event, at)
}
func TestWriteFenceCoversUIProjectionReconciliation(t *testing.T) {
	store := syntheticGuardStore(t)
	p := newGuardTestProvider()
	shared := New(calendar.NewRegistry([]calendar.Provider{p}, calendar.RegistryOptions{WriteGuard: store}))
	model := &blockingReadModel{stubEventReadModel: &stubEventReadModel{}, entered: make(chan struct{}), resume: make(chan struct{})}
	ui := shared.CloneWithEventReadModel(model, storage.SyncWindow{})
	done := make(chan error, 1)
	go func() {
		_, _, err := ui.CreateEventWithReconciliation(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
		done <- err
	}()
	<-model.entered
	_, err := shared.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
	if !errors.Is(err, calendar.ErrWriteDispatchBlocked) {
		t.Fatalf("write crossed projection reconciliation: %v", err)
	}
	close(model.resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
