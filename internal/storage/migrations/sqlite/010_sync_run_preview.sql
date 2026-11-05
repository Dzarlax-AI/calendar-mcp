CREATE TABLE sync_run_previews (run_id TEXT PRIMARY KEY REFERENCES sync_runs(id) ON DELETE CASCADE, expires_at TIMESTAMP NOT NULL, payload BLOB);
CREATE INDEX sync_run_previews_expiry_idx ON sync_run_previews(expires_at) WHERE payload IS NOT NULL;
