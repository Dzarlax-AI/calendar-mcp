CREATE TABLE calendar_display_settings (id INTEGER PRIMARY KEY CHECK (id = 1), hide_declined BOOLEAN NOT NULL DEFAULT FALSE);
INSERT INTO calendar_display_settings (id, hide_declined) VALUES (1, FALSE);
