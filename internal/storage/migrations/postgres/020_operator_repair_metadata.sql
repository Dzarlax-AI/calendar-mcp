CREATE TABLE operator_repair_mutations (
 operation_id TEXT PRIMARY KEY REFERENCES projection_mutation_operations(operation_id) ON DELETE RESTRICT,
 calendar_id TEXT NOT NULL,
 object_id TEXT NOT NULL,
 etag TEXT NOT NULL
);
