ALTER TABLE sync_rules ADD COLUMN config_version INTEGER NOT NULL DEFAULT 1 CHECK (config_version > 0);

-- Runtime scheduling fields do not change the rule's configuration. State is
-- included so pause/resume invalidates work prepared before either change.
CREATE TRIGGER sync_rules_config_version_after_update
AFTER UPDATE OF source_calendar_id, target_calendar_id, state, interval_seconds,
  lookback_days, lookahead_days, recurrence_mode, copy_attendees, notification_policy
ON sync_rules
WHEN OLD.source_calendar_id IS NOT NEW.source_calendar_id
  OR OLD.target_calendar_id IS NOT NEW.target_calendar_id
  OR OLD.state IS NOT NEW.state
  OR OLD.interval_seconds IS NOT NEW.interval_seconds
  OR OLD.lookback_days IS NOT NEW.lookback_days
  OR OLD.lookahead_days IS NOT NEW.lookahead_days
  OR OLD.recurrence_mode IS NOT NEW.recurrence_mode
  OR OLD.copy_attendees IS NOT NEW.copy_attendees
  OR OLD.notification_policy IS NOT NEW.notification_policy
BEGIN
  UPDATE sync_rules SET config_version = OLD.config_version + 1 WHERE id = OLD.id;
END;
