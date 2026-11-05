package storage

import (
	"errors"
	"testing"
)

func TestCacheExecutionEligibilityAliasesAndLegacy(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	if err := s.CreateConnection(ctx, Connection{ID: "other", Provider: "google", AccountFingerprint: "other", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Calendar{{ID: "target", ConnectionID: "connection", ProviderCalendarID: "physical-target", Name: "target", CanRead: true, CanWrite: true, DiscoveredAt: now}, {ID: "alias", ConnectionID: "other", ProviderCalendarID: "physical-target", Name: "alias", CanRead: true, CanWrite: true, DiscoveredAt: now}, {ID: "independent", ConnectionID: "other", ProviderCalendarID: "independent", Name: "other", CanRead: true, CanWrite: true, DiscoveredAt: now}} {
		if err := s.UpsertCalendar(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calendars SET provider_calendar_id='physical-source' WHERE id='calendar'`); err != nil {
		t.Fatal(err)
	}
	mk := func(id, target, mode, state string) Rule {
		return Rule{ID: id, SourceCalendarID: "calendar", TargetCalendarID: target, State: state, SyncMode: mode, IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	}
	if err := s.CreateRule(ctx, mk("cache", "target", "cached_instances", "paused")); err != nil {
		t.Fatal(err)
	}
	check := func(id string, want bool) {
		t.Helper()
		err := s.CheckSyncTargetEligibility(ctx, id)
		if (err == nil) != want {
			t.Fatalf("eligibility %s: %v", id, err)
		}
		if err != nil && !errors.Is(err, ErrCacheExecutionIneligible) {
			t.Fatal(err)
		}
	}
	check("cache", true)
	own := Mapping{ID: "own-legacy", RuleID: "cache", ObjectKind: "event", SourceEventID: "old-source", TargetEventID: "old-target", ReconciliationState: "legacy", LastSeenAt: now}
	if err := s.UpsertMapping(ctx, own); err != nil {
		t.Fatal(err)
	}
	check("cache", false)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM event_mappings WHERE id=?`, own.ID); err != nil {
		t.Fatal(err)
	}
	check("cache", true)
	if err := s.CreateRule(ctx, mk("other-rule", "independent", "direct", "enabled")); err != nil {
		t.Fatal(err)
	}
	check("cache", true)
	if err := s.CreateRule(ctx, mk("alias-rule", "alias", "direct", "enabled")); err != nil {
		t.Fatal(err)
	}
	check("cache", false)
	check("alias-rule", false)
	if _, err := s.db.ExecContext(ctx, `UPDATE sync_rules SET state='paused' WHERE id='alias-rule'`); err != nil {
		t.Fatal(err)
	}
	check("cache", true)
	check("alias-rule", false)
	mapping := Mapping{ID: "legacy", RuleID: "alias-rule", ObjectKind: "event", SourceEventID: "source-event", TargetEventID: "target-event", ReconciliationState: "legacy", LastSeenAt: now}
	if err := s.UpsertMapping(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	check("cache", false)
	if _, err := s.db.ExecContext(ctx, `UPDATE calendars SET provider_calendar_id='primary' WHERE id='target'`); err != nil {
		t.Fatal(err)
	}
	check("cache", false)
	if _, err := s.db.ExecContext(ctx, `UPDATE calendars SET connection_id='other' WHERE id='calendar'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calendars SET provider_calendar_id='physical-source' WHERE id='target'`); err != nil {
		t.Fatal(err)
	}
	check("cache", false)
	if _, err := s.db.ExecContext(ctx, `UPDATE calendars SET provider_calendar_id='' WHERE id='target'`); err != nil {
		t.Fatal(err)
	}
	check("cache", false)
}

func TestCacheEligibilityRejectsCrossAccountAliasCycle(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	if err := s.CreateConnection(ctx, Connection{ID: "other", Provider: "google", AccountFingerprint: "other", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calendars SET provider_calendar_id='physical-a' WHERE id='calendar'`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Calendar{{ID: "b", ConnectionID: "connection", ProviderCalendarID: "physical-b", Name: "b", CanRead: true, CanWrite: true, DiscoveredAt: now}, {ID: "a-alias", ConnectionID: "other", ProviderCalendarID: "physical-a", Name: "aa", CanRead: true, CanWrite: true, DiscoveredAt: now}, {ID: "b-alias", ConnectionID: "other", ProviderCalendarID: "physical-b", Name: "bb", CanRead: true, CanWrite: true, DiscoveredAt: now}} {
		if err := s.UpsertCalendar(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(id, source, target, mode, state string) Rule {
		return Rule{ID: id, SourceCalendarID: source, TargetCalendarID: target, SyncMode: mode, State: state, IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	}
	if err := s.CreateRule(ctx, rule("cache", "calendar", "b", "cached_instances", "paused")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRule(ctx, rule("direct", "b-alias", "a-alias", "direct", "enabled")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cache", "direct"} {
		if err := s.CheckSyncTargetEligibility(ctx, id); !errors.Is(err, ErrCacheExecutionIneligible) {
			t.Fatalf("cycle accepted for %s: %v", id, err)
		}
	}
}

func TestSyncModeChangesAdvanceRuleVersion(t *testing.T) {
	s, now := newEventCacheStore(t)
	ctx := t.Context()
	if err := s.UpsertCalendar(ctx, Calendar{ID: "target", ConnectionID: "connection", ProviderCalendarID: "target", Name: "target", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	r := Rule{ID: "rule", SourceCalendarID: "calendar", TargetCalendarID: "target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	before, err := s.RuleConfigVersion(ctx, "rule")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE sync_rules SET sync_mode='cached_instances' WHERE id='rule'`); err != nil {
		t.Fatal(err)
	}
	after, err := s.RuleConfigVersion(ctx, "rule")
	if err != nil || after != before+1 {
		t.Fatalf("version: %d -> %d, %v", before, after, err)
	}
}
