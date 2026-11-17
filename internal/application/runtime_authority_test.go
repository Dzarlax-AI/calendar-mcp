package application

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type delayedAuthorityProvider struct {
	stubV2Provider
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (p *delayedAuthorityProvider) RouteName() string           { return "google@authority" }
func (p *delayedAuthorityProvider) OwnsCalendar(id string) bool { return id == "primary" }
func (p *delayedAuthorityProvider) delay(ctx context.Context) error {
	p.calls.Add(1)
	close(p.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		return nil
	}
}
func (p *delayedAuthorityProvider) CreateEventV2(ctx context.Context, r calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	if r.Notifications != calendar.NotificationsNone {
		panic("synthetic mutation notifications must be disabled")
	}
	if err := p.delay(ctx); err != nil {
		return nil, err
	}
	return p.stubV2Provider.CreateEventV2(ctx, r)
}
func (p *delayedAuthorityProvider) UpdateEventV2(ctx context.Context, r calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if r.Notifications != calendar.NotificationsNone {
		panic("synthetic mutation notifications must be disabled")
	}
	if err := p.delay(ctx); err != nil {
		return nil, err
	}
	return p.stubV2Provider.UpdateEventV2(ctx, r)
}
func (p *delayedAuthorityProvider) DeleteEventV2(ctx context.Context, r calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if r.Notifications != calendar.NotificationsNone {
		panic("synthetic mutation notifications must be disabled")
	}
	if err := p.delay(ctx); err != nil {
		return nil, err
	}
	return p.stubV2Provider.DeleteEventV2(ctx, r)
}

func TestActualStoreApplicationRejectsLateProjectionAfterRegrant(t *testing.T) {
	for _, operation := range []string{"create", "update_series_related", "delete_series"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "synthetic.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			const cid = "google:authority:primary"
			if err = store.CreateConnection(ctx, storage.Connection{ID: "authority", Provider: "google", DisplayName: "Synthetic authority", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			window := storage.SyncWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
			grant := func() {
				t.Helper()
				snapshot, err := store.BeginAuthorization(ctx, "authority", nil, now)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.CompleteGoogleAuthorization(ctx, snapshot, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: cid, ConnectionID: "authority", ProviderCalendarID: "primary", Name: "Synthetic primary"}, AccessRole: "owner"}}, now); err != nil {
					t.Fatal(err)
				}
				if err = store.EnsureCalendarSyncStates(ctx, now, window); err != nil {
					t.Fatal(err)
				}
			}
			grant()
			event := calendar.EventV2{ID: "series", CalendarID: "primary", Provider: "google", Title: "OLD PRIVATE TITLE", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
			related := event
			related.ID = "related"
			provider := &delayedAuthorityProvider{stubV2Provider: stubV2Provider{name: "google", capabilities: calendar.CalendarCapabilities{MutationScopes: []calendar.MutationScope{calendar.ScopeSeries}, NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}, created: &event, updated: &calendar.OperationResult{Status: "completed", Event: &event, RelatedEvents: []calendar.EventV2{related}}}, entered: make(chan struct{}), release: make(chan struct{})}
			service := New(calendar.NewRegistry([]calendar.Provider{provider})).CloneWithEventReadModel(store, window)
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "create":
					var result *calendar.EventV2
					var warnings []string
					result, warnings, err = service.CreateEventWithReconciliation(ctx, calendar.CreateEventRequestV2{CalendarID: cid, Notifications: calendar.NotificationsNone, Event: calendar.EventCreateV2{Start: event.Start, End: event.End}})
					if err == nil && (result == nil || result.Title != event.Title || len(warnings) != 1) {
						err = fmt.Errorf("provider result=%+v warnings=%v", result, warnings)
					}
				case "update_series_related":
					_, err = service.UpdateEvent(ctx, calendar.UpdateEventRequestV2{Ref: calendar.EventRef{CalendarID: cid, EventID: "google:series"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone})
				case "delete_series":
					_, err = service.DeleteEvent(ctx, calendar.DeleteEventRequestV2{Ref: calendar.EventRef{CalendarID: cid, EventID: "google:series"}, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone})
				}
				done <- err
			}()
			select {
			case <-provider.entered:
			case err := <-done:
				t.Fatalf("provider not entered: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Regrant before releasing the old response: a check of current
			// CanRead/generation at write time would incorrectly accept it.
			grant()
			fresh := event
			fresh.CalendarID = cid
			fresh.Title = "NEW AUTHORIZATION TITLE"
			ticket, err := store.CaptureWriterTicket(ctx, cid)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.UpsertCachedEventWithTicket(ctx, ticket, fresh, now); err != nil {
				t.Fatal(err)
			}
			close(provider.release)
			if err = <-done; err != nil {
				t.Fatalf("preserve provider result: %v", err)
			}
			if provider.calls.Load() != 1 {
				t.Fatalf("provider mutation replayed: %d calls", provider.calls.Load())
			}
			rows, _, err := store.ListCachedEvents(ctx, []string{cid}, window.Start, window.End)
			if err != nil || len(rows) != 1 || rows[0].Title != fresh.Title {
				t.Fatalf("stale projection rows=%+v error=%v", rows, err)
			}
		})
	}
}
