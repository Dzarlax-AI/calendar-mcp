package storage

import (
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// SQLite stores historical text and configured integer timestamps together.
// The normalized UTC key preserves nanoseconds and applies the configured
// timezone to legacy offset-free values.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("calendar_instant_key", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if args[0] == nil {
			return nil, nil
		}
		unit, ok := args[1].(int64)
		if !ok {
			return nil, fmt.Errorf("invalid SQLite timestamp unit")
		}
		zone, ok := args[2].(string)
		if !ok {
			return nil, fmt.Errorf("invalid SQLite timestamp timezone")
		}
		location, err := timeLocation(zone)
		if err != nil {
			return nil, err
		}
		value := cachedStatusTime{unit: unit, location: location}
		if err := value.Scan(args[0]); err != nil {
			return nil, err
		}
		instant := value.Time.UTC()
		if instant.Year() < 0 || instant.Year() > 9999 {
			return nil, fmt.Errorf("SQLite timestamp outside supported year range")
		}
		return instant.Format("2006-01-02T15:04:05.000000000Z"), nil
	})
}

func timeLocation(zone string) (*time.Location, error) {
	if zone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(zone)
}

func (s *Store) sqliteInstantKey(expression string) string {
	zone := ""
	if s.sqliteTimeLocation != nil {
		zone = s.sqliteTimeLocation.String()
	}
	return "calendar_instant_key(" + expression + "," + strconv.FormatInt(s.sqliteTimeUnit, 10) + ",'" + strings.ReplaceAll(zone, "'", "''") + "')"
}

// Only transform explicitly enumerated event-sync comparisons. Other SQL,
// including the default SQLite and PostgreSQL query shapes, is unchanged.
func (s *Store) eventSyncTimeComparisons(query string) string {
	if s.dialect != DialectSQLite || s.sqliteTimeUnit == 0 {
		return query
	}
	for _, column := range []string{"ss.next_sync_at", "ss.lease_until", "next_sync_at", "lease_until"} {
		for _, op := range []string{"<=", ">=", "<", ">"} {
			old := column + op + "?"
			query = strings.ReplaceAll(query, old, s.sqliteInstantKey(column)+op+s.sqliteInstantKey("?"))
		}
	}
	return query
}
