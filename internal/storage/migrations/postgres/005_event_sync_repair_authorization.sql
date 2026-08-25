ALTER TABLE calendar_sync_quarantine
  ADD COLUMN provider_mutation_authorized_etag TEXT NOT NULL DEFAULT '';
