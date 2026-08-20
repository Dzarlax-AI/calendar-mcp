package providers

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"

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
		var provider calendar.Provider
		switch record.Provider {
		case "google":
			if f.config.GoogleClientID == "" || f.config.GoogleClientSecret == "" {
				return nil, fmt.Errorf("build google connection %q: application credentials are not configured", record.ID)
			}
			provider, err = google.NewWithTokenStore(f.config.GoogleClientID, f.config.GoogleClientSecret,
				f.connections.OAuthTokenStore(record.ID, record.Provider), &oauth2.Token{})
		case "microsoft":
			if f.config.MS365ClientID == "" || f.config.MS365ClientSecret == "" || f.config.MS365TenantID == "" {
				return nil, fmt.Errorf("build microsoft connection %q: application credentials are not configured", record.ID)
			}
			provider, err = microsoft.NewWithTokenStore(f.config.MS365ClientID, f.config.MS365ClientSecret, f.config.MS365TenantID,
				f.connections.OAuthTokenStore(record.ID, record.Provider), &oauth2.Token{})
		case "apple":
			var appleCredentials connections.AppleCredentials
			appleCredentials, err = f.connections.LoadApple(ctx, record.ID)
			if err == nil {
				provider, err = apple.New(appleCredentials.Username, appleCredentials.AppPassword, f.config.AppleCalDAVURL)
			}
		default:
			return nil, fmt.Errorf("unsupported stored provider %q", record.Provider)
		}
		if err != nil {
			return nil, fmt.Errorf("build %s connection %q: %w", record.Provider, record.ID, err)
		}
		if routed, ok := provider.(calendar.RouteConfigurableProvider); ok {
			routed.SetRoute(record.Provider+"@"+record.ID, calendarIDs[record.ID])
		}
		result = append(result, provider)
	}
	return result, nil
}
