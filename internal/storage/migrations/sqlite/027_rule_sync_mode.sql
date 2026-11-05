ALTER TABLE sync_rules ADD COLUMN sync_mode TEXT NOT NULL DEFAULT 'direct' CHECK(sync_mode IN ('direct','cached_instances'));
CREATE TRIGGER sync_rules_mode_version_after_update
AFTER UPDATE OF sync_mode ON sync_rules
WHEN OLD.sync_mode IS NOT NEW.sync_mode
BEGIN
 UPDATE sync_rules SET config_version = config_version + 1 WHERE id = OLD.id;
END;
