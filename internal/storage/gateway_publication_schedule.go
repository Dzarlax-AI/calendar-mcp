package storage

import (
	"context"
	"time"
)

// GatewayPublicationCandidates derives pending work from durable sync and grant
// state. A scheduler can walk by calendar ID so a failing source does not
// delay other candidates indefinitely. Selection confers no permission to
// publish: the ordinary capture and commit fences still apply.
func (s *Store) GatewayPublicationCandidates(ctx context.Context, after string, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT c.id FROM calendars c
	JOIN connections a ON a.id=c.connection_id
	JOIN calendar_sync_state ss ON ss.calendar_id=c.id
	JOIN gateway_calendar_grants g ON g.calendar_id=c.id AND g.connection_id=a.id AND g.detail_receipt=c.detail_receipt
	JOIN gateway_account_checks ac ON ac.connection_id=a.id AND ac.authorization_revision=a.authorization_revision
	LEFT JOIN gateway_event_publications p ON p.calendar_id=c.id
	LEFT JOIN projection_mutation_guards mg ON mg.calendar_id=c.id
	WHERE c.enabled=TRUE AND c.can_read=TRUE AND a.status='connected'
	AND ss.status='ready' AND ss.last_success_at IS NOT NULL
	AND g.access_class<>'metadata' AND ac.verified_until_ms>0 AND g.verified_until_ms>?
	AND NOT EXISTS (SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=c.id)
	AND (p.calendar_id IS NULL OR p.sync_generation<>ss.generation OR p.grant_revision<>g.grant_revision
		OR p.mutation_epoch<>COALESCE(mg.epoch,0) OR p.observed_ms<=?)
	ORDER BY CASE WHEN c.id>? THEN 0 ELSE 1 END, c.id`), now.UnixMilli(), now.Add(-GatewayAuthorityLifetime+GatewayAuthorityInterval).UnixMilli(), after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
