package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// The preserved legacy experiment also used version 28, but did not add the
// stop-request column. An already-stamped database must not silently pass.
func TestConflictingVersion28FailsClosed(t *testing.T) {
	s := openSQLiteForMigration(t, 28)
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

func openSQLiteForMigration(t *testing.T, version int) *Store {
	t.Helper()
	ctx := t.Context()
	s, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for next := 1; next <= version; next++ {
		script, err := migrations.ReadFile(fmt.Sprintf("migrations/sqlite/%03d_%s.sql", next, migrationName(next)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, next, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestSQLiteAuthorityMigration28To29(t *testing.T) {
	runAuthorityMigration28To29(t, openSQLiteForMigration(t, 28))
}

func runAuthorityMigration28To29(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	verified := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	assertSchemaVersion(t, store, 28)
	_, err := store.db.ExecContext(ctx, store.query(`INSERT INTO connections(id,provider,account_fingerprint,display_name,status,encrypted_credentials,credential_version,scopes_json,last_verified_at,created_at,updated_at)
		VALUES('legacy','google','legacy','Synthetic legacy','connected',?,1,'[]',?,?,?)`), []byte("synthetic"), verified, verified, verified)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(ctx, store.query(`INSERT INTO calendars(id,connection_id,provider_calendar_id,name,can_read,can_write,supports_recurrence,discovered_at,enabled,google_primary)
		VALUES('google:legacy:primary','legacy','primary','Synthetic calendar',TRUE,TRUE,TRUE,?,FALSE,TRUE)`), verified)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, store, SchemaVersion)
	con, err := store.ConnectionByID(ctx, "legacy")
	if err != nil || con.AuthorizationRevision != "" || con.LastVerifiedAt == nil || !verified.Equal(*con.LastVerifiedAt) {
		t.Fatalf("legacy account proof=%q, verified=%v, error=%v", con.AuthorizationRevision, con.LastVerifiedAt, err)
	}
	calendars, err := store.ListCalendars(ctx, "legacy")
	if err != nil || len(calendars) != 1 || calendars[0].DetailReceipt != "" || calendars[0].Enabled || !calendars[0].GooglePrimary {
		t.Fatalf("legacy selection or authority changed: %+v, error=%v", calendars, err)
	}
	if err = store.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
