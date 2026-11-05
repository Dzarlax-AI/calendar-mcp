CREATE TABLE projection_revision_provenance (
 calendar_id TEXT NOT NULL,
 revision BIGINT NOT NULL,
 observations BYTEA NOT NULL,
 PRIMARY KEY(calendar_id, revision),
 FOREIGN KEY(calendar_id, revision) REFERENCES projection_snapshots(calendar_id, revision) ON DELETE CASCADE
);
