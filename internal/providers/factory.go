package providers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/google"
	"calendar-mcp/internal/microsoft"
	"calendar-mcp/internal/storage"
)

type Factory struct {
	config      *config.Config
	store       *storage.Store
	connections *connections.Service
}

func NewFactory(cfg *config.Config, store *storage.Store, service *connections.Service) *Factory {
	return &Factory{config: cfg, store: store, connections: service}
}

func (f *Factory) Build(ctx context.Context) ([]calendar.Provider, error) {
	records, err := f.store.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	discovered, err := f.store.ListAllCalendars(ctx)
	if err != nil {
		return nil, err
	}
	calendarIDs := map[string][]string{}
	for _, item := range discovered {
		calendarIDs[item.ConnectionID] = append(calendarIDs[item.ConnectionID], item.ProviderCalendarID)
	}
	result := make([]calendar.Provider, 0, len(records))
	for _, record := range records {
		if record.Status != "connected" {
			continue
		}
		snapshot, snapshotErr := f.store.CredentialSnapshot(ctx, record.ID)
		if snapshotErr != nil {
			if errors.Is(snapshotErr, storage.ErrNotFound) && (record.Provider == "microsoft" || record.Provider == "google") {
				log.Printf("calendar %s route skipped: credential snapshot unavailable", record.Provider)
				continue
			}
			return nil, snapshotErr
		}
		if snapshot.Status != "connected" {
			continue
		}
		var provider calendar.Provider
		switch record.Provider {
		case "google":
			if f.config.GoogleClientID == "" || f.config.GoogleClientSecret == "" {
				continue
			}
			provider, err = google.NewWithSnapshotTokenStore(f.config.GoogleClientID, f.config.GoogleClientSecret,
				f.connections.SnapshotOAuthTokenStore(snapshot))
		case "microsoft":
			if f.config.MS365ClientID == "" || f.config.MS365ClientSecret == "" || f.config.MS365TenantID == "" {
				continue
			}
			provider, err = f.BuildNonGoogleVerification(snapshot)
			if err != nil {
				// An already captured snapshot can fail only local construction.
				// Keep the remaining routes available for healthy connections.
				log.Print("calendar microsoft route skipped: invalid credential snapshot")
				continue
			}
		case "apple":
			var appleCredentials connections.AppleCredentials
			appleCredentials, err = f.connections.LoadAppleSnapshot(snapshot)
			if err == nil {
				provider, err = apple.New(appleCredentials.Username, appleCredentials.AppPassword, f.config.AppleCalDAVURL)
			}
		default:
			return nil, fmt.Errorf("unsupported stored provider %q", record.Provider)
		}
		if err != nil {
			return nil, fmt.Errorf("build %s connection %q: %w", record.Provider, record.ID, err)
		}
		if gp, ok := provider.(*google.Provider); ok {
			f.attachGoogleRoleObserver(gp, snapshot.ConnectionID)
		}
		if bound, ok := provider.(interface{ SetCredentialBinding(string, [32]byte) }); ok {
			bound.SetCredentialBinding(snapshot.AuthorizationRevision, sha256.Sum256(snapshot.EncryptedCredentials))
		}
		if routed, ok := provider.(calendar.RouteConfigurableProvider); ok {
			routed.SetRoute(record.Provider+"@"+record.ID, calendarIDs[record.ID])
		}
		result = append(result, provider)
	}
	return result, nil
}

// BuildGoogleVerification constructs only from the caller's captured snapshot;
// it does not re-read credentials or admit pending providers into the registry.
func (f *Factory) BuildGoogleVerification(snapshot storage.CredentialSnapshot) (calendar.Provider, error) {
	if snapshot.Provider != "google" || f.config.GoogleClientID == "" || f.config.GoogleClientSecret == "" {
		return nil, fmt.Errorf("Google verification unavailable")
	}
	p, err := google.NewWithSnapshotTokenStore(f.config.GoogleClientID, f.config.GoogleClientSecret, f.connections.SnapshotOAuthTokenStore(snapshot))
	if err != nil {
		return nil, err
	}
	p.SetRoute("google@"+snapshot.ConnectionID, nil)
	f.attachGoogleRoleObserver(p, snapshot.ConnectionID)
	return p, nil
}

// BuildNonGoogleVerification binds verification to exactly the captured credentials.
func (f *Factory) BuildNonGoogleVerification(snapshot storage.CredentialSnapshot) (calendar.Provider, error) {
	var p calendar.Provider
	var err error
	switch snapshot.Provider {
	case "apple":
		var c connections.AppleCredentials
		c, err = f.connections.LoadAppleSnapshot(snapshot)
		if err == nil {
			p, err = apple.New(c.Username, c.AppPassword, f.config.AppleCalDAVURL)
		}
	case "microsoft":
		if f.config.MS365ClientID == "" || f.config.MS365ClientSecret == "" || f.config.MS365TenantID == "" {
			return nil, errors.New("Microsoft verification unavailable")
		}
		p, err = microsoft.NewWithSnapshotTokenStore(f.config.MS365ClientID, f.config.MS365ClientSecret, f.config.MS365TenantID, f.connections.SnapshotOAuthTokenStore(snapshot))
	default:
		return nil, errors.New("non-Google verification unavailable")
	}
	if err != nil {
		return nil, err
	}
	if routed, ok := p.(calendar.RouteConfigurableProvider); ok {
		routed.SetRoute(snapshot.Provider+"@"+snapshot.ConnectionID, nil)
	}
	return p, nil
}

// BootstrapOneLegacyGoogle performs at most one closed, one-shot migration per cycle.
func (f *Factory) BootstrapOneLegacyGoogle(ctx context.Context, now time.Time) error {
	snapshot, err := f.store.BeginOneLegacyGoogleAuthorization(ctx, now)
	if err != nil || snapshot == nil {
		return err
	}
	return f.connections.VerifyGoogleSnapshot(ctx, *snapshot, f.BuildGoogleVerification)
}

// RefreshProvider returns the current connected credential snapshot on the next
// call. It never retries an operation already sent through an older provider.
func (f *Factory) RefreshProvider(ctx context.Context, old calendar.Provider) (calendar.Provider, error) {
	route := calendar.ProviderRouteName(old)
	provider := old.Name()
	if provider != "google" && provider != "microsoft" && provider != "apple" {
		return old, nil
	}
	id, platform := strings.CutPrefix(route, provider+"@")
	if !platform {
		return old, nil
	}
	snapshot, err := f.store.CredentialSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	if snapshot.Status != "connected" || snapshot.Provider != provider {
		return nil, storage.ErrAuthorityChanged
	}
	digest := sha256.Sum256(snapshot.EncryptedCredentials)
	if bound, ok := old.(interface{ CredentialBinding() (string, [32]byte) }); ok {
		revision, previous := bound.CredentialBinding()
		if revision == snapshot.AuthorizationRevision && previous == digest {
			return old, nil
		}
	}
	var p calendar.Provider
	switch provider {
	case "google":
		p, err = f.BuildGoogleVerification(snapshot)
	case "microsoft", "apple":
		p, err = f.BuildNonGoogleVerification(snapshot)
	}
	if err != nil {
		return nil, err
	}
	if bound, ok := p.(interface{ SetCredentialBinding(string, [32]byte) }); ok {
		bound.SetCredentialBinding(snapshot.AuthorizationRevision, digest)
	}
	calendars, err := f.store.ListCalendars(ctx, id)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(calendars))
	for _, c := range calendars {
		ids = append(ids, c.ProviderCalendarID)
	}
	if routed, ok := p.(calendar.RouteConfigurableProvider); ok {
		routed.SetRoute(route, ids)
	}
	return p, nil
}

// DiscoverGoogleRoutes constructs only missing, durably verified Google routes.
// It performs no provider I/O; every returned snapshot is rechecked before use.
func (f *Factory) DiscoverGoogleRoutes(ctx context.Context, id string, known map[string]bool) ([]calendar.Provider, error) {
	if f.config.GoogleClientID == "" || f.config.GoogleClientSecret == "" {
		return nil, nil
	}
	ids, err := f.store.ReadyGoogleRouteIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	var result []calendar.Provider
	for _, id := range ids {
		if known["google@"+id] {
			continue
		}
		snapshot, err := f.store.CredentialSnapshot(ctx, id)
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrAuthorityChanged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if snapshot.Provider != "google" || snapshot.Status != "connected" {
			continue
		}

		p, err := f.BuildGoogleVerification(snapshot)
		if err != nil {
			return nil, err
		}
		calendars, err := f.store.ListCalendars(ctx, id)
		if err != nil {
			return nil, err
		}
		var calendarIDs []string
		for _, c := range calendars {
			calendarIDs = append(calendarIDs, c.ProviderCalendarID)
		}
		p.(calendar.RouteConfigurableProvider).SetRoute("google@"+id, calendarIDs)
		p.(interface{ SetCredentialBinding(string, [32]byte) }).SetCredentialBinding(snapshot.AuthorizationRevision, sha256.Sum256(snapshot.EncryptedCredentials))
		if err := f.store.ValidateCredentialSnapshot(ctx, snapshot); err != nil {
			if errors.Is(err, storage.ErrAuthorityChanged) || errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return nil, err
		}
		ready, err := f.store.ReadyGoogleRouteIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(ready) == 0 {
			continue
		}
		result = append(result, p)
	}
	return result, nil
}
