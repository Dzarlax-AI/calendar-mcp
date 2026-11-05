CREATE TABLE projection_rule_checkpoints (
 rule_id TEXT PRIMARY KEY REFERENCES sync_rules(id) ON DELETE RESTRICT,
 checkpoint_version BIGINT NOT NULL CHECK(checkpoint_version > 0),
 calendar_id TEXT NOT NULL,
 revision BIGINT NOT NULL,
 window_start TIMESTAMPTZ NOT NULL,
 window_end TIMESTAMPTZ NOT NULL,
 rule_version BIGINT NOT NULL,
 job_id TEXT NOT NULL,
 job_attempt INTEGER NOT NULL,
 worker_id TEXT NOT NULL,
 pin_id TEXT NOT NULL REFERENCES projection_snapshot_pins(pin_id) ON DELETE RESTRICT,
 recorded_at TIMESTAMPTZ NOT NULL,
 FOREIGN KEY(calendar_id,revision) REFERENCES projection_snapshots(calendar_id,revision) ON DELETE RESTRICT
);
