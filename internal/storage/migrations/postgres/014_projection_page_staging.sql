CREATE TABLE projection_page_attempts (
 calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
 generation BIGINT NOT NULL,
 attempt_id TEXT NOT NULL,
 lease_owner TEXT NOT NULL,
 mode TEXT NOT NULL,
 base_revision BIGINT NOT NULL,
 base_cursor TEXT NOT NULL,
 guard_epoch BIGINT NOT NULL,
 window_start TIMESTAMPTZ NOT NULL,
 window_end TIMESTAMPTZ NOT NULL,
 timezone TEXT NOT NULL,
 next_ordinal INTEGER NOT NULL DEFAULT 0,
 next_token_hash TEXT NOT NULL,
 total_bytes BIGINT NOT NULL DEFAULT 0,
 complete BOOLEAN NOT NULL DEFAULT FALSE,
 final_cursor TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (calendar_id, generation),
 UNIQUE (calendar_id, attempt_id)
);
CREATE TABLE projection_page_attempt_pages (
 calendar_id TEXT NOT NULL,
 generation BIGINT NOT NULL,
 page_ordinal INTEGER NOT NULL,
 page_hash TEXT NOT NULL,
 previous_token_hash TEXT NOT NULL,
 next_token_hash TEXT NOT NULL,
 payload BYTEA NOT NULL,
 PRIMARY KEY (calendar_id, generation, page_ordinal),
 FOREIGN KEY (calendar_id, generation) REFERENCES projection_page_attempts(calendar_id, generation) ON DELETE CASCADE
);
