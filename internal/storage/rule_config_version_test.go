package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteRuleConfigVersion(t *testing.T) {
	s := newSQLiteStore(t)
	// The migration is applied by the normal runner once it is registered.
	// Until then, install it locally so this contract is independently testable.
	var columns int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('sync_rules') WHERE name='config_version'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns == 0 {
		script, err := migrations.ReadFile("migrations/sqlite/013_rule_config_version.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(t.Context(), string(script)); err != nil {
			t.Fatal(err)
		}
	}
	runRuleConfigVersionContract(t, s)
}

// This contract can also run against a migrated Postgres store.
func runRuleConfigVersionContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateConnection(ctx, Connection{ID: "version-account", Provider: "google", AccountFingerprint: "version-account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"version-source", "version-target"} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "version-account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	rule := Rule{ID: "version-rule", SourceCalendarID: "version-source", TargetCalendarID: "version-target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	version := func() int64 {
		t.Helper()
		n, err := s.RuleConfigVersion(ctx, rule.ID)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := version(); n != 1 {
		t.Fatalf("initial version = %d", n)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_rules SET next_run_at=?,updated_at=? WHERE id=?`), now, now.Add(time.Minute), rule.ID); err != nil {
		t.Fatal(err)
	}
	if n := version(); n != 1 {
		t.Fatalf("schedule update changed version: %d", n)
	}
	unchanged := now.Add(5 * time.Minute)
	if err := s.SetRuleStateAtVersion(ctx, rule.ID, "paused", 1, &unchanged, now); !errors.Is(err, ErrRuleConfigChanged) {
		t.Fatalf("no-op transition = %v", err)
	}
	if err := s.SetRuleStateAtVersion(ctx, rule.ID, "enabled", 1, nil, now); err != nil {
		t.Fatal(err)
	}
	if n := version(); n != 2 {
		t.Fatalf("enable version = %d", n)
	}
	if err := s.SetRuleStateAtVersion(ctx, rule.ID, "paused", 1, nil, now); !errors.Is(err, ErrRuleConfigChanged) {
		t.Fatalf("stale transition = %v", err)
	}
	if err := s.SetRuleState(ctx, rule.ID, "paused", nil, now); err != nil {
		t.Fatal(err)
	}
	if n := version(); n != 3 {
		t.Fatalf("legacy pause version = %d", n)
	}
	if err := s.SetRuleState(ctx, rule.ID, "paused", nil, now); err != nil {
		t.Fatal(err)
	}
	if n := version(); n != 3 {
		t.Fatalf("no-op changed version: %d", n)
	}
	if _, err := s.db.ExecContext(ctx, s.query(`UPDATE sync_rules SET lookahead_days=15 WHERE id=?`), rule.ID); err != nil {
		t.Fatal(err)
	}
	if n := version(); n != 4 {
		t.Fatalf("config update version = %d", n)
	}
	if err := s.SetRuleStateAtVersion(ctx, rule.ID, "enabled", 3, nil, now); !errors.Is(err, ErrRuleConfigChanged) {
		t.Fatalf("stale config transition = %v", err)
	}
	if err := s.SetRuleStateAtVersion(ctx, "missing", "paused", 1, nil, now); !errors.Is(err, ErrRuleConfigChanged) {
		t.Fatalf("missing transition = %v", err)
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- s.SetRuleStateAtVersion(ctx, rule.ID, "enabled", 4, nil, now) }()
	}
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRuleConfigChanged):
			conflicts++
		default:
			t.Fatalf("concurrent transition: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 || version() != 5 {
		t.Fatalf("concurrent results: successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestSQLiteRuleVersionMigrationPreservesExistingRule(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "before.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	initial, err := migrations.ReadFile("migrations/sqlite/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO connections(id,provider,account_fingerprint,display_name,status,encrypted_credentials,credential_version,created_at,updated_at) VALUES('old','google','old','old','connected',X'00',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-source", "old-target"} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO calendars(id,connection_id,provider_calendar_id,name,can_read,can_write,supports_recurrence,discovered_at) VALUES(?,'old',?,?,1,1,0,?)`, id, id, id, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sync_rules(id,source_calendar_id,target_calendar_id,state,interval_seconds,lookback_days,lookahead_days,recurrence_mode,copy_attendees,notification_policy,created_at,updated_at) VALUES('old-rule','old-source','old-target','enabled',600,7,30,'preserve',0,'none',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	script, err := migrations.ReadFile("migrations/sqlite/013_rule_config_version.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(script)); err != nil {
		t.Fatal(err)
	}
	var state string
	var lookback, lookahead int
	var version int64
	if err := s.db.QueryRowContext(ctx, `SELECT state,lookback_days,lookahead_days,config_version FROM sync_rules WHERE id='old-rule'`).Scan(&state, &lookback, &lookahead, &version); err != nil {
		t.Fatal(err)
	}
	if state != "enabled" || lookback != 7 || lookahead != 30 || version != 1 {
		t.Fatalf("migration changed existing rule: state=%s lookback=%d lookahead=%d version=%d", state, lookback, lookahead, version)
	}
}
