CREATE TABLE cache_event_mappings (
 rule_id TEXT NOT NULL REFERENCES sync_rules(id) ON DELETE RESTRICT,
 identity_hash TEXT NOT NULL,
 mapping_version BIGINT NOT NULL CHECK (mapping_version > 0),
 source_calendar_id TEXT NOT NULL,
 source_revision BIGINT NOT NULL,
 target_calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE RESTRICT,
 target_event_id TEXT NOT NULL,
 ownership_marker TEXT NOT NULL,
 target_etag TEXT NOT NULL,
 content_hash TEXT NOT NULL,
 rule_version BIGINT NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY (rule_id, identity_hash),
 UNIQUE (target_calendar_id, target_event_id),
 UNIQUE (target_calendar_id, ownership_marker),
 FOREIGN KEY (source_calendar_id, source_revision) REFERENCES projection_snapshots(calendar_id, revision) ON DELETE RESTRICT
);
