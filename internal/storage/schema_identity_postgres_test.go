//go:build integration

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestPostgresConflictingVersion28FailsClosed(t *testing.T) {
	raw := os.Getenv("TEST_POSTGRES_URL")
	if raw == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("schema_identity_%d", time.Now().UnixNano())
	if _, err = admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = admin.Close()
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), `ALTER TABLE sync_jobs DROP COLUMN stop_requested`); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckSchema(t.Context()); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("schema accepted by startup: %v", err)
	}
	if err = s.Migrate(t.Context()); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("schema accepted by migration: %v", err)
	}
	var version int
	if err = s.db.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 28 {
		t.Fatalf("migration history changed: %d %v", version, err)
	}
}
