CREATE TABLE calendar_sync_quarantine (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  object_id TEXT NOT NULL,
  etag TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL,
  first_seen_at TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL,
  next_repair_at TIMESTAMPTZ NOT NULL,
  repair_attempts INTEGER NOT NULL DEFAULT 0,
  active BOOLEAN NOT NULL DEFAULT TRUE,
  PRIMARY KEY (calendar_id, object_id)
);
CREATE INDEX calendar_sync_quarantine_due_idx ON calendar_sync_quarantine(calendar_id, next_repair_at) WHERE active = TRUE;
