CREATE TABLE target_operation_intents (
  operation_id TEXT PRIMARY KEY,
  rule_id TEXT NOT NULL REFERENCES sync_rules(id) ON DELETE RESTRICT,
  source_calendar_id TEXT NOT NULL,
  target_calendar_id TEXT NOT NULL,
  source_revision BIGINT NOT NULL,
  window_start TIMESTAMPTZ NOT NULL,
  window_end TIMESTAMPTZ NOT NULL,
  identity_hash TEXT NOT NULL,
  target_event_id TEXT NOT NULL,
  expected_etag TEXT NOT NULL,
  ownership_marker TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  operation_kind TEXT NOT NULL CHECK(operation_kind IN ('create','update','delete')),
  rule_version BIGINT NOT NULL,
  job_id TEXT NOT NULL,
  job_attempt INTEGER NOT NULL,
  worker_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','ambiguous')),
  created_at TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX target_operation_one_unresolved ON target_operation_intents(rule_id,identity_hash) WHERE status IN ('pending','ambiguous');
CREATE INDEX target_operation_rule_idx ON target_operation_intents(rule_id);
