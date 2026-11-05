//go:build integration

package eventsync

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"calendar-mcp/internal/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresBackgroundSyncPublishesCacheCopySource(t *testing.T) {
	raw := os.Getenv("TEST_POSTGRES_URL")
	if raw == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("source_sync_%d", time.Now().UnixNano())
	if _, err = admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = admin.Close()
	})
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	s, err := storage.Open(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	runBackgroundSyncPublishesCacheCopySource(t, s)
}
