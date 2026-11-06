package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrRuleConfigChanged means that a rule transition could not be applied with
// the supplied version. It also covers a missing rule or ineligible calendars;
// callers must re-read state instead of inferring the cause.
var ErrRuleConfigChanged = errors.New("rule configuration changed or rule is unavailable")

// RuleConfigVersion returns the version of a rule's configuration and state.
// It is a point-in-time observation, not authorization to write to a provider.
// An eventual executor must verify this value together with its job claim and
// calendar eligibility in the same transaction as its durable mutation intent.
func (s *Store) RuleConfigVersion(ctx context.Context, ruleID string) (int64, error) {
	if ruleID == "" {
		return 0, errors.New("rule ID is required")
	}
	var version int64
	err := s.db.QueryRowContext(ctx, s.query(`SELECT config_version FROM sync_rules WHERE id=?`), ruleID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read rule configuration version: %w", err)
	}
	return version, nil
}

// SetRuleStateAtVersion performs a single conditional state transition. The
// database bumps config_version when state actually changes, including a later
// pause/resume. An already-equal state fails the conditional transition.
// This does not fence any previously issued provider call.
func (s *Store) SetRuleStateAtVersion(ctx context.Context, id, state string, expectedVersion int64, nextRunAt *time.Time, updatedAt time.Time) error {
	if id == "" || expectedVersion < 1 || updatedAt.IsZero() {
		return errors.New("invalid rule transition")
	}
	if state != "paused" && state != "enabled" {
		return errors.New("rule state must be paused or enabled")
	}
	result, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_rules SET state=?,next_run_at=?,updated_at=? WHERE id=? AND config_version=? AND state<>? AND state<>'archived' AND
        (? <> 'enabled' OR NOT EXISTS (SELECT 1 FROM sync_jobs WHERE rule_id=sync_rules.id AND (state='unknown' OR (state='running' AND stop_requested=TRUE))) AND NOT EXISTS (SELECT 1 FROM exclusive_dispatch_reservations WHERE released_at IS NULL) AND EXISTS (SELECT 1 FROM calendars source JOIN calendars target ON target.id=sync_rules.target_calendar_id
        WHERE source.id=sync_rules.source_calendar_id AND source.enabled=TRUE AND target.enabled=TRUE))`), state, nextRunAt, updatedAt, id, expectedVersion, state, state)
	if err != nil {
		return fmt.Errorf("conditionally set rule state: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect conditional rule transition: %w", err)
	}
	if changed != 1 {
		return ErrRuleConfigChanged
	}
	return nil
}
