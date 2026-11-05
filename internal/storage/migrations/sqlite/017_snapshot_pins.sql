CREATE TABLE projection_snapshot_pins (
 pin_id TEXT PRIMARY KEY,
 calendar_id TEXT NOT NULL,
 revision BIGINT NOT NULL,
 rule_id TEXT NOT NULL REFERENCES sync_rules(id) ON DELETE RESTRICT,
 rule_version BIGINT NOT NULL,
 job_id TEXT NOT NULL,
 job_attempt INTEGER NOT NULL,
 worker_id TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('active','released')),
 created_at TIMESTAMP NOT NULL,
 expires_at TIMESTAMP NOT NULL,
 released_at TIMESTAMP,
 UNIQUE(rule_id,job_id,job_attempt),
 FOREIGN KEY(calendar_id,revision) REFERENCES projection_snapshots(calendar_id,revision) ON DELETE RESTRICT
);
CREATE INDEX projection_snapshot_pins_rule_idx ON projection_snapshot_pins(rule_id);
