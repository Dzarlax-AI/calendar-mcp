CREATE TABLE projection_mutation_operations (
 operation_id TEXT PRIMARY KEY,
 calendar_ids TEXT NOT NULL,
 started_at TIMESTAMPTZ NOT NULL,
 resolved_at TIMESTAMPTZ
);
CREATE TABLE projection_mutation_operation_members (
 operation_id TEXT NOT NULL REFERENCES projection_mutation_operations(operation_id),
 calendar_id TEXT NOT NULL,
 intent_id TEXT NOT NULL CHECK(intent_id = ':' || operation_id),
 PRIMARY KEY(operation_id, calendar_id),
 FOREIGN KEY(calendar_id, intent_id) REFERENCES projection_mutation_intents(calendar_id, mutation_id) ON DELETE RESTRICT
);
