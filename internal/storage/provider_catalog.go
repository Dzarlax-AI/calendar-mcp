package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ConfiguredProviderRoutes reads connection identities/status without reading
// credentials or performing discovery writes. A disconnected connection makes
// a supposedly complete availability catalog unsafe.
func (s *Store) ConfiguredProviderRoutes(ctx context.Context, provider string) ([]string, error) {
	if provider != "google" && provider != "microsoft" && provider != "apple" {
		return nil, fmt.Errorf("unsupported provider")
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT id,status FROM connections WHERE provider=? ORDER BY id`), provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := []string{}
	for rows.Next() {
		var id, status string
		if err = rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		if id == "" || status != "connected" {
			return nil, fmt.Errorf("provider connection unavailable")
		}
		routes = append(routes, provider+"@"+id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return routes, nil
}

// EnabledProviderCalendars returns registered enabled IDs only. Missing IDs in
// a provider response make availability completeness unprovable.
func (s *Store) EnabledProviderCalendars(ctx context.Context, provider string) ([]string, error) {
	if provider != "google" && provider != "microsoft" && provider != "apple" {
		return nil, fmt.Errorf("unsupported provider")
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT c.id FROM calendars c JOIN connections cn ON cn.id=c.connection_id WHERE cn.provider=? AND c.enabled=TRUE ORDER BY c.id`), provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		if id == "" {
			return nil, fmt.Errorf("invalid enabled calendar")
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// AvailabilityCalendarSelection distinguishes a registered disabled calendar
// from an unknown one, without registering newly discovered calendars.
func (s *Store) AvailabilityCalendarSelection(ctx context.Context, id string) (bool, bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, s.query(`SELECT enabled FROM calendars WHERE id=?`), id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, enabled, nil
}
