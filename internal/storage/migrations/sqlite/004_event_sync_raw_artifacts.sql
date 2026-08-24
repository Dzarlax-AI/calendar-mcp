CREATE TABLE calendar_sync_raw_artifacts (
  calendar_id TEXT NOT NULL,
  object_id TEXT NOT NULL,
  etag TEXT NOT NULL DEFAULT '',
  payload_ciphertext BLOB NOT NULL,
  payload_sha256 TEXT NOT NULL,
  content_type TEXT NOT NULL DEFAULT '',
  provider_status INTEGER NOT NULL DEFAULT 0,
  provider_reason TEXT NOT NULL DEFAULT '',
  truncated INTEGER NOT NULL DEFAULT 0,
  captured_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  PRIMARY KEY (calendar_id, object_id, etag),
  FOREIGN KEY (calendar_id, object_id) REFERENCES calendar_sync_quarantine(calendar_id, object_id) ON DELETE CASCADE
);
CREATE INDEX calendar_sync_raw_artifacts_expiry_idx ON calendar_sync_raw_artifacts(expires_at);
