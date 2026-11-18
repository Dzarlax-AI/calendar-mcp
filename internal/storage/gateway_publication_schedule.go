package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// NextGatewayPublication derives pending work from durable sync and grant
// state. A scheduler can walk by calendar ID so a failing source does not
// delay other candidates indefinitely. Selection confers no permission to
// publish: the ordinary capture and commit fences still apply.
func (s *Store) NextGatewayPublication(ctx context.Context, after string, now time.Time) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT c.id FROM calendars c
	JOIN connections a ON a.id=c.connection_id
	JOIN calendar_sync_state ss ON ss.calendar_id=c.id
	JOIN gateway_calendar_grants g ON g.calendar_id=c.id AND g.connection_id=a.id AND g.detail_receipt=c.detail_receipt
	JOIN gateway_account_checks ac ON ac.connection_id=a.id AND ac.authorization_revision=a.authorization_revision
	LEFT JOIN gateway_event_publications p ON p.calendar_id=c.id
	LEFT JOIN projection_mutation_guards mg ON mg.calendar_id=c.id
	WHERE c.id>? AND c.enabled=TRUE AND c.can_read=TRUE AND a.status='connected'
	AND ss.status='ready' AND ss.last_success_at IS NOT NULL
	AND g.access_class<>'metadata' AND ac.verified_until_ms>0 AND g.verified_until_ms>?
	AND NOT EXISTS (SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=c.id)
	AND (p.calendar_id IS NULL OR p.sync_generation<>ss.generation OR p.grant_revision<>g.grant_revision
		OR p.mutation_epoch<>COALESCE(mg.epoch,0) OR p.observed_ms<=?)
	ORDER BY c.id LIMIT 1`), after, now.UnixMilli(), now.Add(-GatewayAuthorityLifetime+GatewayAuthorityInterval).UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
