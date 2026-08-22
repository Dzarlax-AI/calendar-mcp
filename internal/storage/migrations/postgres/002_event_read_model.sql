CREATE TABLE cached_events (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  event_id TEXT NOT NULL,
  source_object_id TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  payload_json JSONB NOT NULL,
  start_at TIMESTAMPTZ,
  end_at TIMESTAMPTZ,
  start_date TEXT,
  end_date TEXT,
  deleted BOOLEAN NOT NULL DEFAULT FALSE,
  sync_generation BIGINT NOT NULL,
  provider_updated_at TIMESTAMPTZ,
  synced_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (calendar_id, event_id)
);
CREATE INDEX cached_events_timed_range_idx ON cached_events(calendar_id, start_at, end_at) WHERE deleted = FALSE;
CREATE INDEX cached_events_date_range_idx ON cached_events(calendar_id, start_date, end_date) WHERE deleted = FALSE;
CREATE INDEX cached_events_source_object_idx ON cached_events(calendar_id, source_object_id);

CREATE TABLE calendar_sync_state (
  calendar_id TEXT PRIMARY KEY REFERENCES calendars(id) ON DELETE CASCADE,
  strategy TEXT NOT NULL,
  cursor TEXT NOT NULL DEFAULT '',
  window_start TIMESTAMPTZ NOT NULL,
  window_end TIMESTAMPTZ NOT NULL,
  generation BIGINT NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  next_sync_at TIMESTAMPTZ NOT NULL,
  last_started_at TIMESTAMPTZ,
  last_success_at TIMESTAMPTZ,
  last_error_code TEXT,
  lease_owner TEXT,
  lease_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX calendar_sync_state_due_idx ON calendar_sync_state(next_sync_at, lease_until);

CREATE TABLE calendar_sync_objects (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  object_id TEXT NOT NULL,
  etag TEXT NOT NULL,
  sync_generation BIGINT NOT NULL,
  PRIMARY KEY (calendar_id, object_id)
);
CREATE INDEX calendar_sync_objects_generation_idx ON calendar_sync_objects(calendar_id, sync_generation);
