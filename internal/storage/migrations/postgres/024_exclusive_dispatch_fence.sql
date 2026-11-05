CREATE TABLE exclusive_dispatch_reservations (
 operation_id TEXT PRIMARY KEY REFERENCES projection_mutation_operations(operation_id) ON DELETE RESTRICT,
 target_key TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 operation_kind TEXT NOT NULL CHECK(operation_kind IN ('create','update','delete','run')),
 created_at TIMESTAMPTZ NOT NULL,
 released_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX exclusive_dispatch_active_target ON exclusive_dispatch_reservations(target_key) WHERE released_at IS NULL;
CREATE TABLE exclusive_dispatch_guard_members (
 operation_id TEXT NOT NULL REFERENCES exclusive_dispatch_reservations(operation_id) ON DELETE RESTRICT,
 calendar_id TEXT NOT NULL,
 PRIMARY KEY(operation_id,calendar_id),
 FOREIGN KEY(operation_id,calendar_id) REFERENCES projection_mutation_operation_members(operation_id,calendar_id) ON DELETE RESTRICT
);
