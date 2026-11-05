//go:build integration

package storage

import (
	"context"
	"os"
	"testing"
)

func TestPostgresRuleConfigVersion(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='sync_rules' AND column_name='config_version'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns == 0 {
		script, err := migrations.ReadFile("migrations/postgres/013_rule_config_version.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sync_rules WHERE id='version-rule'`)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM calendars WHERE connection_id='version-account'`)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM connections WHERE id='version-account'`)
	})
	runRuleConfigVersionContract(t, s)
}
