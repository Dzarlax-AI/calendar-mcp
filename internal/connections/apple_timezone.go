package connections

import (
	"context"
	"errors"
	"fmt"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

// RefreshAppleCalendarTimezones refreshes collection metadata for explicitly
// selected sources under the same credentials and source grants.
func (s *Service) RefreshAppleCalendarTimezones(ctx context.Context, connectionID string, calendarIDs []string, build func(storage.CredentialSnapshot) (calendar.Provider, error)) (int, error) {
	if connectionID == "" || len(calendarIDs) == 0 || len(calendarIDs) > 16 || build == nil {
		return 0, errors.New("select one Apple connection and 1 to 16 calendar IDs")
	}
	snapshot, err := s.store.CredentialSnapshot(ctx, connectionID)
	if err != nil {
		return 0, err
	}
	if snapshot.Provider != "apple" || snapshot.Status != "connected" {
		return 0, storage.ErrAuthorityChanged
	}
	stored, err := s.store.ListCalendars(ctx, connectionID)
	if err != nil {
		return 0, err
	}
	eligible := make(map[string]storage.Calendar, len(stored))
	for _, item := range stored {
		if item.Enabled && item.CanRead && item.ProviderCalendarID != "" &&
			item.ID == calendar.AccountCalendarID("apple", connectionID, item.ProviderCalendarID) {
			eligible[item.ID] = item
		}
	}
	targets := make(map[string]int, len(calendarIDs))
	updates := make([]storage.AppleCalendarTimezoneUpdate, 0, len(calendarIDs))
	for _, id := range calendarIDs {
		item, ok := eligible[id]
		if !ok {
			return 0, storage.ErrAuthorityChanged
		}
		if _, duplicate := targets[item.ProviderCalendarID]; duplicate {
			return 0, errors.New("duplicate Apple calendar ID")
		}
		targets[item.ProviderCalendarID] = len(updates)
		updates = append(updates, storage.AppleCalendarTimezoneUpdate{Before: item})
	}

	provider, err := build(snapshot)
	if err != nil {
		return 0, err
	}
	if provider == nil || provider.Name() != "apple" {
		return 0, errors.New("connection provider mismatch")
	}
	discovered, err := provider.ListCalendars(ctx)
	if err != nil {
		return 0, fmt.Errorf("discover Apple calendar timezones: %w", err)
	}
	for _, item := range discovered {
		i, selected := targets[item.ID]
		if !selected {
			continue
		}
		location := calendar.CollectionLocation("apple", item.Timezone)
		if updates[i].Timezone != "" || location == nil {
			return 0, errors.New("Apple calendar timezone could not be verified uniquely")
		}
		// An IANA match over sampled transitions does not establish coverage
		// for the whole cached window. Limit this repair to fixed definitions.
		start, end := s.now().In(location).ZoneBounds()
		if !start.IsZero() || !end.IsZero() {
			return 0, errors.New("Apple calendar timezone refresh requires a fixed offset")
		}
		updates[i].Timezone = item.Timezone
	}
	for _, update := range updates {
		if update.Timezone == "" {
			return 0, errors.New("selected Apple calendar was absent from CalDAV discovery")
		}
	}
	return s.store.UpdateAppleCalendarTimezones(ctx, snapshot, updates)
}
