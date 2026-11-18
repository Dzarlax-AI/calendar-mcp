ALTER TABLE connections ADD COLUMN authorization_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE calendars ADD COLUMN detail_receipt TEXT NOT NULL DEFAULT '';
