CREATE UNIQUE INDEX target_intent_operation_calendar_idx ON target_operation_intents(operation_id,target_calendar_id);
CREATE TABLE target_operation_guard_links (
 operation_id TEXT NOT NULL,
 calendar_id TEXT NOT NULL,
 PRIMARY KEY(operation_id),
 FOREIGN KEY(operation_id,calendar_id) REFERENCES target_operation_intents(operation_id,target_calendar_id) ON DELETE RESTRICT,
 FOREIGN KEY(operation_id,calendar_id) REFERENCES projection_mutation_operation_members(operation_id,calendar_id) ON DELETE RESTRICT
);
