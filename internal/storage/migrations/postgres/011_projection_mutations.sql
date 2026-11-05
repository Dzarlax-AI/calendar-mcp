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
  window_start TIMESTAMPTZ NOT NULL,
  window_end TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL,
  timezone TEXT NOT NULL,
  cursor TEXT NOT NULL,
  payload BYTEA NOT NULL
);
CREATE TABLE projection_snapshots (
  calendar_id TEXT NOT NULL REFERENCES projection_mutation_guards(calendar_id) ON DELETE CASCADE,
  revision BIGINT NOT NULL,
  stage_id TEXT NOT NULL,
  guard_epoch BIGINT NOT NULL,
  base_revision BIGINT NOT NULL,
  window_start TIMESTAMPTZ NOT NULL,
  window_end TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL,
  timezone TEXT NOT NULL,
  cursor TEXT NOT NULL,
  payload BYTEA NOT NULL,
  verified BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY(calendar_id, revision),
  UNIQUE(calendar_id, stage_id)
);
CREATE TABLE projection_mutation_intents (
  calendar_id TEXT NOT NULL REFERENCES projection_mutation_guards(calendar_id) ON DELETE CASCADE,
  mutation_id TEXT NOT NULL,
  started_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (calendar_id, mutation_id)
);

-- This protects revision contents from UPDATE, not privileged direct INSERT.
-- A future trusted finalizer may insert verified=true only after its checks.
CREATE FUNCTION projection_snapshot_verified_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'projection snapshot revision is immutable';
END;
$$;
CREATE TRIGGER projection_snapshot_verified_immutable BEFORE UPDATE ON projection_snapshots
FOR EACH ROW EXECUTE FUNCTION projection_snapshot_verified_immutable();
