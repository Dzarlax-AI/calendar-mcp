CREATE TABLE calendar_sync_provider_corrections (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  object_id TEXT NOT NULL,
  outcome TEXT NOT NULL,
  corrected_at TIMESTAMP NOT NULL,
  PRIMARY KEY (calendar_id, object_id, corrected_at)
);

CREATE INDEX calendar_sync_provider_corrections_recent_idx
  ON calendar_sync_provider_corrections(corrected_at DESC);
