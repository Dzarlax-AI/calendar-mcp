package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newSQLiteStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSQLiteStorageContract(t *testing.T) {
	store := newSQLiteStore(t)
	runStorageContract(t, store)
}

func TestAllProviderPairingsAreAllowed(t *testing.T) {
	providers := []string{"google", "microsoft", "apple"}
	for _, sourceProvider := range providers {
		for _, targetProvider := range providers {
			name := sourceProvider + "_to_" + targetProvider
			t.Run(name, func(t *testing.T) {
				store := newSQLiteStore(t)
				ctx := context.Background()
				now := time.Now().UTC()
				for _, id := range []string{"source-account", "target-account"} {
					provider := sourceProvider
					if id == "target-account" {
						provider = targetProvider
					}
					if err := store.CreateConnection(ctx, Connection{ID: id, Provider: provider, DisplayName: id, Status: "connected", EncryptedCredentials: []byte("cipher"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.UpsertCalendar(ctx, Calendar{ID: "source", ConnectionID: "source-account", ProviderCalendarID: "source", Name: "Source", CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: now}); err != nil {
					t.Fatal(err)
				}
				if err := store.UpsertCalendar(ctx, Calendar{ID: "target", ConnectionID: "target-account", ProviderCalendarID: "target", Name: "Target", CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: now}); err != nil {
					t.Fatal(err)
				}
				rule := Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
				if err := store.CreateRule(ctx, rule); err != nil {
					t.Fatalf("pairing rejected: %v", err)
				}
			})
		}
	}
}

func TestCreateRuleRejectsDirectAndTransitiveCycles(t *testing.T) {
	store := newSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateConnection(ctx, Connection{ID: "account", Provider: "google", DisplayName: "Google", Status: "connected", EncryptedCredentials: []byte("cipher"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := store.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	newRule := func(id, source, target string) Rule {
		return Rule{ID: id, SourceCalendarID: source, TargetCalendarID: target, State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	}
	if err := store.CreateRule(ctx, newRule("a-b", "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRule(ctx, newRule("b-a", "b", "a")); !errors.Is(err, ErrRuleCycle) {
		t.Fatalf("direct cycle error = %v", err)
	}
	if err := store.CreateRule(ctx, newRule("b-c", "b", "c")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRule(ctx, newRule("c-a", "c", "a")); !errors.Is(err, ErrRuleCycle) {
		t.Fatalf("transitive cycle error = %v", err)
	}
}

func runStorageContract(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	for _, c := range []Connection{
		{ID: "ms", Provider: "microsoft", DisplayName: "Microsoft", Status: "connected", EncryptedCredentials: []byte("cipher-ms"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now},
		{ID: "google", Provider: "google", DisplayName: "Google", Status: "connected", EncryptedCredentials: []byte("cipher-google"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.CreateConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ConnectionByProvider(ctx, "apple"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing connection error = %v", err)
	}
	connections, err := store.ListConnections(ctx)
	if err != nil || len(connections) != 2 {
		t.Fatalf("connections = %d, err = %v", len(connections), err)
	}
	if err := store.CreateConnection(ctx, connections[0]); err == nil {
		t.Fatal("duplicate provider succeeded")
	}

	for _, c := range []Calendar{
		{ID: "source", ConnectionID: "ms", ProviderCalendarID: "source-provider", Name: "Source", CanRead: true, SupportsRecurrence: true, DiscoveredAt: now},
		{ID: "target", ConnectionID: "google", ProviderCalendarID: "target-provider", Name: "Target", CanRead: true, CanWrite: true, SupportsRecurrence: true, DiscoveredAt: now},
	} {
		if err := store.UpsertCalendar(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	calendars, err := store.ListCalendars(ctx, "ms")
	if err != nil || len(calendars) != 1 || calendars[0].ID != "source" {
		t.Fatalf("calendars = %#v, err = %v", calendars, err)
	}

	rule := Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target", State: "paused", IntervalSeconds: 600,
		LookbackDays: 0, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].LookaheadDays != 14 {
		t.Fatalf("rules = %#v, err = %v", rules, err)
	}
	invalid := rule
	invalid.ID, invalid.LookaheadDays = "invalid", 0
	if err := store.CreateRule(ctx, invalid); err == nil {
		t.Fatal("invalid depth succeeded")
	}

	job := Job{ID: "job", RuleID: "rule", Kind: "dry_run", State: "pending", AvailableAt: now, CreatedAt: now}
	if err := store.EnqueueJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, "worker-1", now.Add(time.Second))
	if err != nil || claimed == nil || claimed.State != "running" || claimed.Attempt != 1 {
		t.Fatalf("claimed = %#v, err = %v", claimed, err)
	}
	parallel := Job{ID: "parallel", RuleID: "rule", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now.Add(time.Second)}
	if err := store.EnqueueJob(ctx, parallel); err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimJob(ctx, "worker-2", now.Add(time.Second))
	if err != nil || again != nil {
		t.Fatalf("second claim = %#v, err = %v", again, err)
	}

	run := Run{ID: "run", JobID: "job", RuleID: "rule", Trigger: "dry_run", Outcome: "running", StartedAt: now, DryRun: true}
	if err := store.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "run", "job", "succeeded", now.Add(time.Minute), Run{CreatedCount: 2, SkippedCount: 1}); err != nil {
		t.Fatal(err)
	}
	claimedParallel, err := store.ClaimJob(ctx, "worker-2", now.Add(time.Minute))
	if err != nil || claimedParallel == nil || claimedParallel.ID != "parallel" {
		t.Fatalf("parallel claim = %#v, err = %v", claimedParallel, err)
	}
	if err := store.FinishRun(ctx, "missing-run", "parallel", "succeeded", now.Add(2*time.Minute), Run{}); err != nil {
		t.Fatal(err)
	}
	stale := Job{ID: "stale", RuleID: "rule", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now.Add(2 * time.Minute)}
	if err := store.EnqueueJob(ctx, stale); err != nil {
		t.Fatal(err)
	}
	claimedStale, err := store.ClaimJob(ctx, "dead-worker", now.Add(3*time.Minute))
	if err != nil || claimedStale == nil || claimedStale.ID != "stale" {
		t.Fatalf("stale claim = %#v, err = %v", claimedStale, err)
	}
	staleRun := Run{ID: "stale-run", JobID: "stale", RuleID: "rule", Trigger: "manual", Outcome: "running", StartedAt: now.Add(3 * time.Minute)}
	if err := store.StartRun(ctx, staleRun); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverStaleJobs(ctx, now.Add(4*time.Minute), now.Add(20*time.Minute)); err != nil || count != 1 {
		t.Fatalf("recovered=%d err=%v", count, err)
	}
	reclaimed, err := store.ClaimJob(ctx, "worker-3", now.Add(20*time.Minute))
	if err != nil || reclaimed == nil || reclaimed.ID != "stale" || reclaimed.Attempt != 2 {
		t.Fatalf("reclaimed = %#v, err = %v", reclaimed, err)
	}
	if err := store.FinishRun(ctx, "missing-run-2", "stale", "succeeded", now.Add(21*time.Minute), Run{}); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRuns(ctx, 10)
	if err != nil || len(runs) != 2 || runs[0].ErrorCode != "worker_lease_expired" || runs[1].CreatedCount != 2 || !runs[1].DryRun {
		t.Fatalf("runs = %#v, err = %v", runs, err)
	}
	if err := store.SetRuleState(ctx, "rule", "enabled", nil, now); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ScheduleDueJobs(ctx, now.Add(2*time.Minute)); err != nil || count != 1 {
		t.Fatalf("scheduled=%d err=%v", count, err)
	}
	if count, err := store.ScheduleDueJobs(ctx, now.Add(2*time.Minute)); err != nil || count != 0 {
		t.Fatalf("duplicate scheduled=%d err=%v", count, err)
	}
	scheduled, err := store.ClaimJob(ctx, "worker-1", now.Add(2*time.Minute))
	if err != nil || scheduled == nil || scheduled.Kind != "scheduled" {
		t.Fatalf("scheduled claim=%#v err=%v", scheduled, err)
	}

	mapping := Mapping{ID: "mapping", RuleID: "rule", ObjectKind: "occurrence", SourceEventID: "source-event",
		OriginalStart: "2026-08-20T10:00:00Z", TargetEventID: "target-event", ContentHash: "one", LastSeenAt: now, ReconciliationState: "current"}
	if err := store.UpsertMapping(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	mapping.ContentHash = "two"
	if err := store.UpsertMapping(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	mappings, err := store.ListMappings(ctx, "rule")
	if err != nil || len(mappings) != 1 || mappings[0].ContentHash != "two" {
		t.Fatalf("mappings = %#v, err = %v", mappings, err)
	}
	if err := store.DeleteConnection(ctx, "ms"); !errors.Is(err, ErrConnectionInUse) {
		t.Fatalf("delete referenced connection error = %v", err)
	}

	attempt := OAuthAttempt{StateHash: "state", Provider: "google", EncryptedVerifier: []byte("cipher"), ReturnPath: "/connections", ExpiresAt: now.Add(time.Minute)}
	if err := store.PutOAuthAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	consumed, err := store.ConsumeOAuthAttempt(ctx, "state", now)
	if err != nil || consumed.ConsumedAt == nil {
		t.Fatalf("consume = %#v, err = %v", consumed, err)
	}
	if _, err := store.ConsumeOAuthAttempt(ctx, "state", now); !errors.Is(err, ErrOAuthNotConsumable) {
		t.Fatalf("replay error = %v", err)
	}
	expired := OAuthAttempt{StateHash: "expired", Provider: "google", EncryptedVerifier: []byte("cipher"), ReturnPath: "/", ExpiresAt: now.Add(-time.Second)}
	if err := store.PutOAuthAttempt(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if count, err := store.DeleteExpiredOAuthAttempts(ctx, now); err != nil || count != 1 {
		t.Fatalf("deleted = %d, err = %v", count, err)
	}
}

func TestParseDatabaseURL(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		dialect Dialect
		driver  string
	}{
		{"sqlite:///tmp/calendar.db", DialectSQLite, "sqlite"},
		{"postgres://localhost/calendar", DialectPostgres, "pgx"},
	} {
		dialect, driver, _, err := parseDatabaseURL(tc.raw)
		if err != nil || dialect != tc.dialect || driver != tc.driver {
			t.Fatalf("parse(%q) = %q, %q, %v", tc.raw, dialect, driver, err)
		}
	}
	if _, _, _, err := parseDatabaseURL("mysql://localhost/db"); err == nil {
		t.Fatal("unsupported URL succeeded")
	}
}
