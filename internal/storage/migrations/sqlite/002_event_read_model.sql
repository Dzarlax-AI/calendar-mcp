CREATE TABLE cached_events (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  event_id TEXT NOT NULL,
  source_object_id TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL,
  start_at TIMESTAMP,
  end_at TIMESTAMP,
  start_date TEXT,
  end_date TEXT,
  deleted INTEGER NOT NULL DEFAULT 0,
  sync_generation INTEGER NOT NULL,
  provider_updated_at TIMESTAMP,
  synced_at TIMESTAMP NOT NULL,
  PRIMARY KEY (calendar_id, event_id)
);
CREATE INDEX cached_events_timed_range_idx ON cached_events(calendar_id, start_at, end_at) WHERE deleted = 0;
CREATE INDEX cached_events_date_range_idx ON cached_events(calendar_id, start_date, end_date) WHERE deleted = 0;
CREATE INDEX cached_events_source_object_idx ON cached_events(calendar_id, source_object_id);

CREATE TABLE calendar_sync_state (
  calendar_id TEXT PRIMARY KEY REFERENCES calendars(id) ON DELETE CASCADE,
  strategy TEXT NOT NULL,
  cursor TEXT NOT NULL DEFAULT '',
  window_start TIMESTAMP NOT NULL,
  window_end TIMESTAMP NOT NULL,
  generation INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  next_sync_at TIMESTAMP NOT NULL,
  last_started_at TIMESTAMP,
  last_success_at TIMESTAMP,
  last_error_code TEXT,
  lease_owner TEXT,
  lease_until TIMESTAMP,
  updated_at TIMESTAMP NOT NULL
);
CREATE INDEX calendar_sync_state_due_idx ON calendar_sync_state(next_sync_at, lease_until);

CREATE TABLE calendar_sync_objects (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  object_id TEXT NOT NULL,
  etag TEXT NOT NULL,
  sync_generation INTEGER NOT NULL,
  PRIMARY KEY (calendar_id, object_id)
);
CREATE INDEX calendar_sync_objects_generation_idx ON calendar_sync_objects(calendar_id, sync_generation);
