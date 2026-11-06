package storage

import (
	"errors"
	"testing"
)

// The preserved legacy experiment also used version 28, but did not add the
// stop-request column. An already-stamped database must not silently pass.
func TestConflictingVersion28FailsClosed(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE sync_jobs DROP COLUMN stop_requested`); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSchema(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("schema accepted by startup: %v", err)
	}
	if err := s.Migrate(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("schema accepted by migration: %v", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 28 {
		t.Fatalf("migration history changed: %d %v", version, err)
	}
}
