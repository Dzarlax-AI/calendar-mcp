CREATE TABLE connections (
  id TEXT PRIMARY KEY, provider TEXT NOT NULL, account_fingerprint TEXT NOT NULL, display_name TEXT NOT NULL,
  status TEXT NOT NULL, encrypted_credentials BYTEA NOT NULL, credential_version INTEGER NOT NULL,
  scopes_json JSONB NOT NULL DEFAULT '[]'::jsonb, last_verified_at TIMESTAMPTZ,
  last_error_code TEXT, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(provider, account_fingerprint)
);
CREATE TABLE calendars (
  id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE RESTRICT,
  provider_calendar_id TEXT NOT NULL, name TEXT NOT NULL, timezone TEXT NOT NULL DEFAULT '',
  can_read BOOLEAN NOT NULL, can_write BOOLEAN NOT NULL, supports_recurrence BOOLEAN NOT NULL,
  discovered_at TIMESTAMPTZ NOT NULL, UNIQUE(connection_id, provider_calendar_id)
);
CREATE TABLE sync_rules (
  id TEXT PRIMARY KEY, source_calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE RESTRICT,
  target_calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE RESTRICT, state TEXT NOT NULL,
  interval_seconds INTEGER NOT NULL DEFAULT 600 CHECK (interval_seconds IN (600, 1800, 3600)), lookback_days INTEGER NOT NULL DEFAULT 0 CHECK (lookback_days BETWEEN 0 AND 365),
  lookahead_days INTEGER NOT NULL DEFAULT 14 CHECK (lookahead_days BETWEEN 1 AND 365), recurrence_mode TEXT NOT NULL,
  copy_attendees BOOLEAN NOT NULL DEFAULT FALSE, notification_policy TEXT NOT NULL DEFAULT 'none', next_run_at TIMESTAMPTZ,
  consecutive_failures INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
  CHECK (source_calendar_id <> target_calendar_id)
);
CREATE TABLE sync_jobs (
  id TEXT PRIMARY KEY, rule_id TEXT NOT NULL REFERENCES sync_rules(id) ON DELETE CASCADE, kind TEXT NOT NULL,
  state TEXT NOT NULL, available_at TIMESTAMPTZ NOT NULL, claimed_at TIMESTAMPTZ, claimed_by TEXT,
  attempt INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL, finished_at TIMESTAMPTZ
);
CREATE INDEX sync_jobs_claim_idx ON sync_jobs(state, available_at, created_at);
CREATE UNIQUE INDEX sync_jobs_one_running_per_rule_idx ON sync_jobs(rule_id) WHERE state = 'running';
CREATE TABLE sync_runs (
  id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES sync_jobs(id) ON DELETE CASCADE,
  rule_id TEXT NOT NULL REFERENCES sync_rules(id) ON DELETE CASCADE, trigger_kind TEXT NOT NULL, outcome TEXT NOT NULL,
  started_at TIMESTAMPTZ NOT NULL, finished_at TIMESTAMPTZ, created_count INTEGER NOT NULL DEFAULT 0,
  updated_count INTEGER NOT NULL DEFAULT 0, deleted_count INTEGER NOT NULL DEFAULT 0, skipped_count INTEGER NOT NULL DEFAULT 0,
  warning_count INTEGER NOT NULL DEFAULT 0, error_code TEXT, error_summary TEXT, dry_run BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE TABLE event_mappings (
  id TEXT PRIMARY KEY, rule_id TEXT NOT NULL REFERENCES sync_rules(id) ON DELETE CASCADE, object_kind TEXT NOT NULL,
  source_event_id TEXT NOT NULL, source_series_id TEXT NOT NULL DEFAULT '', original_start TEXT NOT NULL DEFAULT '',
  target_event_id TEXT NOT NULL, target_series_id TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL, reconciliation_state TEXT NOT NULL,
  UNIQUE(rule_id, source_event_id, original_start)
);
CREATE UNIQUE INDEX event_mappings_target_idx ON event_mappings(rule_id, target_event_id);
CREATE TABLE oauth_attempts (
  state_hash TEXT PRIMARY KEY, provider TEXT NOT NULL, connection_id TEXT, mode TEXT NOT NULL, encrypted_verifier BYTEA NOT NULL,
  return_path TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL, consumed_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL);
