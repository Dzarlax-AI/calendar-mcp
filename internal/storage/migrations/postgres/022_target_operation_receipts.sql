CREATE TABLE target_operation_receipts (
 operation_id TEXT PRIMARY KEY REFERENCES target_operation_intents(operation_id) ON DELETE RESTRICT,
 request_hash TEXT NOT NULL,
 operation_kind TEXT NOT NULL CHECK(operation_kind IN ('create','update')),
 target_calendar_id TEXT NOT NULL,
 target_event_id TEXT NOT NULL,
 expected_etag TEXT NOT NULL,
 ownership_marker TEXT NOT NULL,
 response_etag TEXT NOT NULL,
 recorded_at TIMESTAMPTZ NOT NULL
);
