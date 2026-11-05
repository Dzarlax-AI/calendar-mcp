package storage

import (
	"calendar-mcp/internal/calendar"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRepairCalendarTimeZone(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateConnection(ctx, Connection{ID: "zone-account", Provider: "google", AccountFingerprint: "zone-account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCalendar(ctx, Calendar{ID: "zone-calendar", ConnectionID: "zone-account", ProviderCalendarID: "primary", Name: "Synthetic", Timezone: "Europe/Belgrade", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	if zone, err := s.RepairCalendarTimeZone(ctx, "zone-calendar"); err != nil || zone != "Europe/Belgrade" {
		t.Fatalf("zone=%q err=%v", zone, err)
	}
	if _, err := s.RepairCalendarTimeZone(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing calendar error = %v", err)
	}
}

func TestSQLiteDiscoveryTimezonePreservation(t *testing.T) {
	s := newSQLiteStore(t)
	runDiscoveryTimezonePreservation(t, s)
}
func runDiscoveryTimezonePreservation(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	id := fmt.Sprintf("discovery-zone-%d", now.UnixNano())
	if err := s.CreateConnection(ctx, Connection{ID: id, Provider: "google", AccountFingerprint: id, Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM calendars WHERE connection_id=?`), id)
		_, _ = s.db.ExecContext(context.Background(), s.query(`DELETE FROM connections WHERE id=?`), id)
	})
	cal := Calendar{ID: calendar.AccountCalendarID("google", id, "owner"), ConnectionID: id, ProviderCalendarID: "owner", Name: "Synthetic", CanRead: true, CanWrite: true, DiscoveredAt: now}
	for _, step := range []struct{ input, want string }{{"Europe/Belgrade", "Europe/Belgrade"}, {"", "Europe/Belgrade"}, {"Local", "Europe/Belgrade"}, {"bad-zone", "Europe/Belgrade"}, {"America/Los_Angeles", "America/Los_Angeles"}} {
		cal.Timezone = step.input
		if err := s.UpsertCalendar(ctx, cal); err != nil {
			t.Fatal(err)
		}
		got, err := s.RepairCalendarTimeZone(ctx, cal.ID)
		if err != nil || got != step.want {
			t.Fatalf("timezone got=%q want=%q err=%v", got, step.want, err)
		}
	}
	item := calendar.Calendar{ID: "discovered", Name: "Synthetic", Timezone: "Asia/Tokyo"}
	if err := s.RegisterDiscoveredCalendar(ctx, "google@"+id, item); err != nil {
		t.Fatal(err)
	}
	got, err := s.RepairCalendarTimeZone(ctx, calendar.AccountCalendarID("google", id, item.ID))
	if err != nil || got != "Asia/Tokyo" {
		t.Fatalf("discovery timezone %q err %v", got, err)
	}
}
