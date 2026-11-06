package calendar

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ProviderRouteCatalog must derive configured account routes from authoritative
// storage without provider credentials or discovery writes.
type ProviderRouteCatalog interface {
	ConfiguredProviderRoutes(context.Context, string) ([]string, error)
	EnabledProviderCalendars(context.Context, string) ([]string, error)
	AvailabilityCalendarSelection(context.Context, string) (bool, bool, error)
}

type AvailabilityCalendarCatalog struct {
	Provider  string     `json:"provider"`
	Complete  bool       `json:"complete"`
	Calendars []Calendar `json:"calendars"`
}

// ListAvailabilityCalendars lists only a selected provider. It compares the
// live provider snapshot to authoritative configured routes, requires every
// configured account to succeed, and never registers discovered calendars.
func (r *Registry) ListAvailabilityCalendars(ctx context.Context, provider string) (AvailabilityCalendarCatalog, error) {
	result := AvailabilityCalendarCatalog{Provider: provider, Calendars: []Calendar{}}
	if provider != "google" && provider != "microsoft" && provider != "apple" {
		return result, fmt.Errorf("unsupported provider")
	}
	catalog, ok := r.selection.(ProviderRouteCatalog)
	if !ok {
		return result, fmt.Errorf("provider inventory unavailable")
	}
	expected, err := catalog.ConfiguredProviderRoutes(ctx, provider)
	if err != nil {
		return result, fmt.Errorf("provider inventory unavailable")
	}
	enabledIDs, err := catalog.EnabledProviderCalendars(ctx, provider)
	if err != nil {
		return result, fmt.Errorf("calendar inventory unavailable")
	}
	expectedSet := map[string]bool{}
	for _, route := range expected {
		prefix, account, ok := strings.Cut(route, "@")
		if !ok || prefix != provider || account == "" || strings.Contains(account, ":") || expectedSet[route] {
			return result, fmt.Errorf("invalid provider inventory")
		}
		expectedSet[route] = true
	}
	providers := r.Providers()
	live := map[string]Provider{}
	for _, p := range providers {
		if p.Name() != provider {
			continue
		}
		route := ProviderRouteName(p)
		if !expectedSet[route] || live[route] != nil {
			return result, fmt.Errorf("provider inventory mismatch")
		}
		live[route] = p
	}
	if len(live) != len(expectedSet) {
		return result, fmt.Errorf("provider inventory mismatch")
	}
	seenIDs := map[string]bool{}
	for _, route := range expected {
		p := live[route]
		cals, err := p.ListCalendars(ctx)
		if err != nil {
			return AvailabilityCalendarCatalog{}, fmt.Errorf("provider listing unavailable")
		}
		for _, item := range cals {
			if item.ID == "" {
				return AvailabilityCalendarCatalog{}, fmt.Errorf("invalid calendar identity")
			}
			known, enabled, err := catalog.AvailabilityCalendarSelection(ctx, CanonicalCalendarID(p, item.ID))
			if err != nil || !known {
				return AvailabilityCalendarCatalog{}, fmt.Errorf("calendar selection unavailable or unregistered")
			}
			if !enabled {
				continue
			}
			if provider == "google" {
				item.ReadOnly = !item.GoogleWriteAccessProven
			}
			item.ID = CanonicalCalendarID(p, item.ID)
			item.Provider = provider
			if seenIDs[item.ID] {
				return AvailabilityCalendarCatalog{}, fmt.Errorf("duplicate calendar identity")
			}
			seenIDs[item.ID] = true
			result.Calendars = append(result.Calendars, item)
		}
	}
	for _, id := range enabledIDs {
		if !seenIDs[id] {
			return AvailabilityCalendarCatalog{}, fmt.Errorf("enabled calendar missing from provider listing")
		}
	}
	currentEnabled, err := catalog.EnabledProviderCalendars(ctx, provider)
	if err != nil {
		return AvailabilityCalendarCatalog{}, fmt.Errorf("calendar inventory unavailable")
	}
	enabledIDs = append([]string(nil), enabledIDs...)
	currentEnabled = append([]string(nil), currentEnabled...)
	sort.Strings(enabledIDs)
	sort.Strings(currentEnabled)
	if len(enabledIDs) != len(currentEnabled) {
		return AvailabilityCalendarCatalog{}, fmt.Errorf("calendar inventory changed")
	}
	for i := range enabledIDs {
		if enabledIDs[i] != currentEnabled[i] {
			return AvailabilityCalendarCatalog{}, fmt.Errorf("calendar inventory changed")
		}
	}
	// Guard against an account being added, removed, or disconnected during
	// listing. No partial results are returned on any mismatch.
	current, err := catalog.ConfiguredProviderRoutes(ctx, provider)
	if err != nil {
		return AvailabilityCalendarCatalog{}, fmt.Errorf("provider inventory unavailable")
	}
	expected = append([]string(nil), expected...)
	current = append([]string(nil), current...)
	sort.Strings(expected)
	sort.Strings(current)
	if len(expected) != len(current) {
		return AvailabilityCalendarCatalog{}, fmt.Errorf("provider inventory changed")
	}
	for i := range expected {
		if expected[i] != current[i] {
			return AvailabilityCalendarCatalog{}, fmt.Errorf("provider inventory changed")
		}
	}
	result.Complete = true
	return result, nil
}
