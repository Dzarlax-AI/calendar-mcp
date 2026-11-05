ALTER TABLE event_mappings ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE event_mappings ADD COLUMN incarnation TEXT NOT NULL DEFAULT '';
ALTER TABLE event_mappings ADD COLUMN target_etag TEXT NOT NULL DEFAULT '';
UPDATE event_mappings SET incarnation='legacy:' || id;
CREATE UNIQUE INDEX event_mappings_incarnation_idx ON event_mappings(incarnation);
