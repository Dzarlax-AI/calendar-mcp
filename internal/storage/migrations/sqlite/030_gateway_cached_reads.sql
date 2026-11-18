CREATE TABLE gateway_account_checks (
  connection_id TEXT PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE,
  authorization_revision TEXT NOT NULL,
  lease_id TEXT NOT NULL DEFAULT '',
  lease_started_ms BIGINT NOT NULL DEFAULT 0,
  lease_until_ms BIGINT NOT NULL DEFAULT 0,
  next_check_ms BIGINT NOT NULL DEFAULT 0,
  verified_until_ms BIGINT NOT NULL DEFAULT 0,
  inventory_revision TEXT NOT NULL DEFAULT ''
);
CREATE INDEX gateway_checks_due_idx ON gateway_account_checks(next_check_ms, lease_until_ms);

CREATE TABLE gateway_calendar_grants (
  calendar_id TEXT PRIMARY KEY REFERENCES calendars(id) ON DELETE CASCADE,
  connection_id TEXT NOT NULL REFERENCES gateway_account_checks(connection_id) ON DELETE CASCADE,
  grant_revision TEXT NOT NULL,
  verified_until_ms BIGINT NOT NULL DEFAULT 0,
  detail_receipt TEXT NOT NULL,
  access_class TEXT NOT NULL CHECK (access_class IN ('metadata', 'events', 'private')),
  metadata_json TEXT NOT NULL,
  capabilities_json TEXT NOT NULL
);
CREATE INDEX gateway_calendar_grants_connection_idx ON gateway_calendar_grants(connection_id, calendar_id);

CREATE TABLE gateway_event_publications (
  calendar_id TEXT PRIMARY KEY REFERENCES calendars(id) ON DELETE CASCADE,
  grant_revision TEXT NOT NULL,
  sync_generation BIGINT NOT NULL,
  mutation_epoch BIGINT NOT NULL,
  content_revision TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  observed_ms BIGINT NOT NULL,
  window_start TEXT NOT NULL,
  window_end TEXT NOT NULL,
  verified_cancellation_series TEXT NOT NULL,
  deletions_complete BOOLEAN NOT NULL
);

CREATE TABLE gateway_event_rows (
  calendar_id TEXT NOT NULL REFERENCES gateway_event_publications(calendar_id) ON DELETE CASCADE,
  event_id TEXT NOT NULL,
  series_id TEXT NOT NULL,
  is_master BOOLEAN NOT NULL,
  is_cancelled BOOLEAN NOT NULL,
  oversized BOOLEAN NOT NULL,
  time_kind TEXT NOT NULL CHECK (time_kind IN ('timed', 'date', 'none')),
  range_start TEXT NOT NULL,
  range_end TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  PRIMARY KEY(calendar_id, event_id)
);
CREATE INDEX gateway_event_range_idx ON gateway_event_rows(calendar_id,time_kind,range_start,range_end);
CREATE INDEX gateway_event_series_idx ON gateway_event_rows(calendar_id,series_id,time_kind,range_start);

CREATE TABLE gateway_writes (
  operation_id TEXT PRIMARY KEY REFERENCES projection_mutation_operations(operation_id) ON DELETE RESTRICT,
  scope_json TEXT NOT NULL,
  grants_json TEXT NOT NULL,
  phase TEXT NOT NULL CHECK (phase IN ('reserved', 'dispatched', 'confirmed', 'not_dispatched', 'outcome_unknown')),
  dispatch_by_ms BIGINT NOT NULL,
  next_check_ms BIGINT NOT NULL,
  outcome_due_ms BIGINT NOT NULL DEFAULT 0,
  confirmed_ms BIGINT NOT NULL DEFAULT 0,
  receipt_ciphertext TEXT NOT NULL DEFAULT ''
);
CREATE INDEX gateway_writes_due_idx ON gateway_writes(next_check_ms);

CREATE TABLE gateway_write_points (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  event_id TEXT NOT NULL,
  operation_id TEXT NOT NULL REFERENCES gateway_writes(operation_id) ON DELETE RESTRICT,
  grant_revision TEXT NOT NULL,
  mutation_epoch BIGINT NOT NULL,
  confirmed_ms BIGINT NOT NULL,
  deleted BOOLEAN NOT NULL,
  oversized BOOLEAN NOT NULL,
  settled BOOLEAN NOT NULL DEFAULT FALSE,
  payload_json TEXT NOT NULL,
  PRIMARY KEY(calendar_id, event_id)
);
