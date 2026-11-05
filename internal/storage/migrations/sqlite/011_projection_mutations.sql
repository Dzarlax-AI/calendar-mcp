CREATE TABLE projection_mutation_guards (
  calendar_id TEXT PRIMARY KEY REFERENCES calendars(id) ON DELETE CASCADE,
  epoch BIGINT NOT NULL DEFAULT 0,
  revision BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE projection_snapshot_staging (
  calendar_id TEXT PRIMARY KEY REFERENCES projection_mutation_guards(calendar_id) ON DELETE CASCADE,
  stage_id TEXT NOT NULL,
  guard_epoch BIGINT NOT NULL,
  base_revision BIGINT NOT NULL,
  window_start TIMESTAMP NOT NULL,
  window_end TIMESTAMP NOT NULL,
  completed_at TIMESTAMP NOT NULL,
  timezone TEXT NOT NULL,
  cursor TEXT NOT NULL,
  payload BLOB NOT NULL
);
CREATE TABLE projection_snapshots (
  calendar_id TEXT NOT NULL REFERENCES projection_mutation_guards(calendar_id) ON DELETE CASCADE,
  revision BIGINT NOT NULL,
  stage_id TEXT NOT NULL,
  guard_epoch BIGINT NOT NULL,
  base_revision BIGINT NOT NULL,
  window_start TIMESTAMP NOT NULL,
  window_end TIMESTAMP NOT NULL,
  completed_at TIMESTAMP NOT NULL,
  timezone TEXT NOT NULL,
  cursor TEXT NOT NULL,
  payload BLOB NOT NULL,
  verified BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY(calendar_id, revision),
  UNIQUE(calendar_id, stage_id)
);
CREATE TABLE projection_mutation_intents (
  calendar_id TEXT NOT NULL REFERENCES projection_mutation_guards(calendar_id) ON DELETE CASCADE,
  mutation_id TEXT NOT NULL,
  started_at TIMESTAMP NOT NULL,
  PRIMARY KEY (calendar_id, mutation_id)
);

-- This protects revision contents from UPDATE, not privileged direct INSERT.
-- A future trusted finalizer may insert verified=true only after its checks.
CREATE TRIGGER projection_snapshot_verified_immutable BEFORE UPDATE ON projection_snapshots
BEGIN
  SELECT RAISE(ABORT, 'projection snapshot revision is immutable');
END;
