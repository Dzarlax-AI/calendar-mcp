package storage

import (
	"context"
	"fmt"
)

// ReadyGoogleRouteIDs returns connected accounts with durable verification. An empty id selects all such accounts in one query.
func (s *Store) ReadyGoogleRouteIDs(ctx context.Context, id string) ([]string, error) {
	q := `SELECT c.id FROM connections c
WHERE c.provider='google' AND c.status='connected' AND c.authorization_revision<>'' AND c.last_verified_at IS NOT NULL AND credential_is_bounded`
	var args []any
	if id != "" {
		q += " AND c.id=?"
		args = append(args, id)
	} else {
		q += " ORDER BY c.id"
	}
	rows, err := s.db.QueryContext(ctx, s.query(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		ids = append(ids, v)
	}
	return ids, rows.Err()
}

// GoogleShortIDOwner selects a unique owner from current discovery, or from
// exact legacy metadata when the read model is disabled.
func (s *Store) GoogleShortIDOwner(ctx context.Context, raw string, allowLegacy bool) (connectionID, revision string, err error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT DISTINCT c.id, c.authorization_revision FROM connections c JOIN calendars cal ON cal.connection_id=c.id
 WHERE c.provider='google' AND c.status='connected' AND credential_is_bounded
 AND ((c.authorization_revision<>'' AND c.last_verified_at IS NOT NULL AND cal.discovered_at=c.last_verified_at)
   OR (? AND c.authorization_revision='' AND cal.can_read=?))
 AND (cal.provider_calendar_id=? OR (?='primary' AND cal.google_primary=?)) ORDER BY c.id LIMIT 2`), allowLegacy, true, raw, raw, true)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", "", rows.Err()
	}
	if err := rows.Scan(&connectionID, &revision); err != nil {
		return "", "", err
	}
	if rows.Next() {
		return "", "", fmt.Errorf("calendar %q is ambiguous across multiple google accounts; use an account-scoped calendar ID", "google:"+raw)
	}
	return connectionID, revision, rows.Err()
}

// ResolveGooglePrimaryCalendarID resolves the alias under the selected current
// account revision so explicit calendar selection uses the canonical row.
func (s *Store) ResolveGooglePrimaryCalendarID(ctx context.Context, connectionID, revision string) (string, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT cal.provider_calendar_id,cal.google_primary
		FROM calendars cal JOIN connections c ON c.id=cal.connection_id
		WHERE c.id=? AND c.authorization_revision=? AND c.provider='google' AND c.status='connected'
		AND (cal.google_primary OR cal.provider_calendar_id='primary')
		ORDER BY cal.google_primary DESC,cal.provider_calendar_id LIMIT 2`), connectionID, revision)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", ErrNotFound
	}
	var raw string
	var primary bool
	if err := rows.Scan(&raw, &primary); err != nil {
		return "", err
	}
	if primary && rows.Next() {
		var second string
		var alsoPrimary bool
		if err := rows.Scan(&second, &alsoPrimary); err != nil {
			return "", err
		}
		if alsoPrimary {
			return "", ErrAuthorityChanged
		}
	}
	return raw, rows.Err()
}
