package storage

import (
	"testing"
	"time"
)

func TestConfiguredProviderRoutesRejectsDisconnected(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := t.Context()
	routes, err := s.ConfiguredProviderRoutes(ctx, "google")
	if err != nil || routes == nil || len(routes) != 0 {
		t.Fatal("empty provider not authoritative")
	}
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, Connection{ID: "account", Provider: "google", DisplayName: "synthetic", Status: "connected", EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	routes, err = s.ConfiguredProviderRoutes(ctx, "google")
	if err != nil || len(routes) != 1 || routes[0] != "google@account" {
		t.Fatal("connected route not returned")
	}
	if err = s.UpsertCalendar(ctx, Calendar{ID: "google:account:enabled", ConnectionID: "account", ProviderCalendarID: "enabled", Name: "synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.EnabledProviderCalendars(ctx, "google")
	if err != nil || len(ids) != 1 || ids[0] != "google:account:enabled" {
		t.Fatal("enabled inventory incorrect")
	}
	if err = s.SetCalendarEnabled(ctx, "google:account:enabled", false, now); err != nil {
		t.Fatal(err)
	}
	ids, err = s.EnabledProviderCalendars(ctx, "google")
	if err != nil || len(ids) != 0 {
		t.Fatal("disabled calendar included")
	}
	known, enabled, err := s.AvailabilityCalendarSelection(ctx, "google:account:enabled")
	if err != nil || !known || enabled {
		t.Fatal("disabled selection indistinguishable")
	}
	known, _, err = s.AvailabilityCalendarSelection(ctx, "google:account:unknown")
	if err != nil || known {
		t.Fatal("unregistered selection accepted")
	}

	if _, err = s.db.ExecContext(ctx, `UPDATE connections SET status='disconnected' WHERE id='account'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfiguredProviderRoutes(ctx, "google"); err == nil {
		t.Fatal("disconnected account accepted as complete")
	}
}
