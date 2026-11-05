package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RepairCalendarTimeZone returns the stored source calendar timezone; callers
// must validate it before using it to interpret civil dates.
func (s *Store) RepairCalendarTimeZone(ctx context.Context, calendarID string) (string, error) {
	if calendarID == "" {
		return "", errors.New("calendar ID is required")
	}
	var zone string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT timezone FROM calendars WHERE id=?`), calendarID).Scan(&zone)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read repair calendar timezone: %w", err)
	}
	return zone, nil
}
