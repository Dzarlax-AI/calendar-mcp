ALTER TABLE sync_rules ADD COLUMN config_version BIGINT NOT NULL DEFAULT 1 CHECK (config_version > 0);

CREATE FUNCTION advance_sync_rule_config_version() RETURNS TRIGGER AS $$
BEGIN
  IF ROW(OLD.source_calendar_id, OLD.target_calendar_id, OLD.state, OLD.interval_seconds,
         OLD.lookback_days, OLD.lookahead_days, OLD.recurrence_mode, OLD.copy_attendees, OLD.notification_policy)
     IS DISTINCT FROM
     ROW(NEW.source_calendar_id, NEW.target_calendar_id, NEW.state, NEW.interval_seconds,
         NEW.lookback_days, NEW.lookahead_days, NEW.recurrence_mode, NEW.copy_attendees, NEW.notification_policy) THEN
    NEW.config_version := OLD.config_version + 1;
  ELSE
    NEW.config_version := OLD.config_version;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER sync_rules_config_version_before_update
BEFORE UPDATE ON sync_rules FOR EACH ROW EXECUTE FUNCTION advance_sync_rule_config_version();
