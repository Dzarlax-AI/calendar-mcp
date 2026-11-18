package connections

import (
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func TestRefreshAppleCalendarTimezonesRejectsNamedZoneWithoutWriting(t *testing.T) {
	service, store := newTestService(t)
	ctx := t.Context()
	const connectionID = "apple-fixture"
	if err := service.CreateApple(ctx, connectionID, "Synthetic", "fixture@example.test", "synthetic"); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{name: "apple", calendars: []calendar.Calendar{
		{ID: "first", Timezone: "UTC"}, {ID: "second", Timezone: "UTC"},
	}}
	build := func(storage.CredentialSnapshot) (calendar.Provider, error) { return provider, nil }
	if err := service.VerifyNonGoogle(ctx, connectionID, build); err != nil {
		t.Fatal(err)
	}
	provider.calendars[0].Timezone = "UTC+05:30:17"
	provider.calendars[1].Timezone = "Europe/Belgrade"
	ids := []string{calendar.AccountCalendarID("apple", connectionID, "first"), calendar.AccountCalendarID("apple", connectionID, "second")}
	if n, err := service.RefreshAppleCalendarTimezones(ctx, connectionID, ids, build); n != 0 || err == nil {
		t.Fatalf("refresh with a named transition zone: %d %v", n, err)
	}
	for _, id := range ids {
		if got, err := store.RepairCalendarTimeZone(ctx, id); err != nil || got != "UTC" {
			t.Fatalf("zone changed: %q %v", got, err)
		}
	}
}
