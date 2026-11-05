CREATE TABLE event_sync_removal_staging (
 calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL,
 page_ordinal INTEGER NOT NULL,
 page_hash TEXT NOT NULL,
 previous_token_hash TEXT NOT NULL,
 next_token_hash TEXT NOT NULL,
 is_final BOOLEAN NOT NULL,
 payload BYTEA NOT NULL,
 PRIMARY KEY (calendar_id, generation, page_ordinal)
);
