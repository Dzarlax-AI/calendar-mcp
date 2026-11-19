package storage

import (
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestGatewayPublicationSQLite(t *testing.T) {
	testGatewayPublication(t, openSQLiteForMigration(t, 29))
}

func testGatewayPublication(t *testing.T, db *Store) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	bounds := SyncWindow{Start: now.Add(-12 * time.Hour), End: now.Add(30 * time.Hour)}
	const account, raw = "capture", "calendar"
	id := calendar.AccountCalendarID("apple", account, raw)
	must(db.CreateConnection(ctx, Connection{ID: account, Provider: "apple", Status: "connected", EncryptedCredentials: []byte("synthetic"),
		CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, now, time.Minute)
	must(err)
	proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "apple", Timezone: "UTC"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "apple"}, Class: GatewayEventAccess}
	must(db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, now.Add(time.Second)))
	must(db.EnsureCalendarSyncStates(ctx, now, bounds))
	state, err := db.ClaimDueCalendarSync(ctx, "synthetic-sync", now, now.Add(time.Minute))
	must(err)
	if state == nil {
		t.Fatal("missing sync")
	}
	next := now.Add(time.Hour)
	must(db.ApplyEventSyncPage(ctx, *state, EventSyncBatch{NextSyncAt: &next}, true, now.Add(time.Second)))
	grantTime := now.Add(2 * time.Second)
	ticket, err := db.CaptureGatewayPublication(ctx, id, grantTime)
	must(err)
	public := calendar.EventV2{ID: "known", Title: "visible", Visibility: "public",
		Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"},
		End:   calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	hidden := public
	hidden.ID, hidden.Title, hidden.Visibility = "hidden", "secret", "private"
	uncertain := public
	uncertain.ID, uncertain.Title, uncertain.Visibility = "unknown", "unknown", "unrecognized"
	must(db.PublishGatewayEvents(ctx, ticket, []calendar.EventV2{hidden, public, uncertain}, true, grantTime, grantTime, nil))
	req := GatewayEventQuery{CalendarIDs: []string{id}, Window: SyncWindow{Start: now.Add(-time.Hour), End: now.Add(2 * time.Hour)},
		CivilStart: "2026-11-18", CivilEnd: "2026-11-19"}
	project := func(at time.Time) GatewayEventRead {
		t.Helper()
		read, err := db.ReadGatewayEvents(ctx, req, at, false)
		must(err)
		return read
	}
	read := project(grantTime)
	if len(read.Items) != 1 || len(read.Sources) != 1 || read.Items[0].Event.Title != "visible" || read.Incomplete {
		t.Fatalf("unexpected authorized projection: %#v", read)
	}
	revision := read.Sources[0].Version
	must(db.PublishGatewayEvents(ctx, ticket, []calendar.EventV2{public, hidden}, true, grantTime.Add(time.Second), grantTime.Add(time.Second), nil))
	if next := project(grantTime.Add(time.Second)).Sources[0].Version; next != revision {
		t.Fatal("unchanged public projection rotated version")
	}
	public.Start.DateTime = now.Add(15 * time.Minute).Format(time.RFC3339)
	must(db.PublishGatewayEvents(ctx, ticket, []calendar.EventV2{public, hidden}, true, grantTime.Add(2*time.Second), grantTime.Add(2*time.Second), nil))
	moved := project(grantTime.Add(2 * time.Second))
	if len(moved.Sources) != 1 || moved.Sources[0].Version == "" || moved.Sources[0].Version == revision ||
		len(moved.Items) != 1 || moved.Items[0].Event.Start.DateTime != public.Start.DateTime {
		t.Fatalf("moving event failed to expose the updated publication: %#v", moved)
	}
	firstGrant, err := db.GatewayCalendarGrant(ctx, id, grantTime)
	must(err)
	proof.DetailReceipt = firstGrant.DetailReceipt
	renewAt := now.Add(5 * time.Minute)
	renewal, err := db.ClaimGatewayAuthorityCheck(ctx, account, renewAt, time.Minute)
	must(err)
	must(db.PublishGatewayAuthority(ctx, renewal, []GatewayCalendarProof{proof}, renewAt.Add(time.Second)))
	rightsStillValid := now.Add(16 * time.Minute)
	_, err = db.GatewayCalendarGrant(ctx, id, rightsStillValid)
	must(err)
	if stale := project(rightsStillValid); len(stale.Items) != 0 || !stale.Incomplete || stale.Sources[0].Source.Status != "stale" {
		t.Fatal("renewing calendar authorization renewed the age of the event fields")
	}

	must(db.BeginProjectionMutationMany(ctx, []string{id}, "synthetic-write-reservation", grantTime.Add(3*time.Second)))
	pending := project(grantTime.Add(3 * time.Second))
	if !pending.Incomplete || len(pending.Items) != 0 {
		t.Fatal("read exposed the pre-mutation snapshot")
	}
	if err := db.PublishGatewayEvents(ctx, ticket, []calendar.EventV2{public}, true, grantTime.Add(4*time.Second), grantTime.Add(4*time.Second), nil); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("late projection crossed mutation fence: %v", err)
	}
	resolvedAt := renewAt.Add(2 * time.Second)
	must(db.ResolveProjectionMutationManyAndSchedule(ctx, []string{id}, "synthetic-write-reservation", resolvedAt))
	state, err = db.ClaimDueCalendarSync(ctx, "replacement", resolvedAt, resolvedAt.Add(time.Minute))
	must(err)
	if state == nil {
		t.Fatal("missing replacement sync after intent cleared")
	}
	at := resolvedAt.Add(time.Second)
	ordinaryDue := at.Add(time.Second)
	must(db.ApplyEventSyncPage(ctx, *state, EventSyncBatch{NextSyncAt: &ordinaryDue}, true, at))
	if err := db.PublishGatewayEvents(ctx, ticket, []calendar.EventV2{public}, true, at, at, nil); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("old capture ticket survived resolved intent and replacement sync: %v", err)
	}

	public.End = public.Start
	fresh, err := db.CaptureGatewayPublication(ctx, id, at)
	must(err)
	must(db.PublishGatewayEvents(ctx, fresh, []calendar.EventV2{public}, true, at, at, nil))
	point := project(at)
	if len(point.Items) != 1 || point.Items[0].Event.Start.DateTime != point.Items[0].Event.End.DateTime {
		t.Fatalf("zero-duration event was lost or changed: %#v", point)
	}

	// Ordinary sync changes the generation even in the absence of a write.
	state, err = db.ClaimDueCalendarSync(ctx, "ordinary-refresh", ordinaryDue, ordinaryDue.Add(time.Minute))
	must(err)
	if state == nil || state.Generation <= fresh.State.Generation {
		t.Fatalf("ordinary sync did not advance the generation: %+v", state)
	}
	readyAt := ordinaryDue.Add(time.Second)
	must(db.ApplyEventSyncPage(ctx, *state, EventSyncBatch{}, true, readyAt))
	if previous := project(readyAt); previous.Incomplete || len(previous.Items) != 1 {
		t.Fatal("ordinary sync interrupted reads of a still-valid publication")
	}
	if err := db.PublishGatewayEvents(ctx, fresh, []calendar.EventV2{public}, true, at, readyAt, nil); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("publish accepted a ticket from before ordinary sync: %v", err)
	}
	queued, err := db.GatewayPublicationCandidates(ctx, id, readyAt)
	must(err)
	if len(queued) != 1 || queued[0] != id {
		t.Fatalf("current generation was not queued after wrapping: %v", queued)
	}
	current, err := db.CaptureGatewayPublication(ctx, id, readyAt)
	must(err)
	must(db.PublishGatewayEvents(ctx, current, []calendar.EventV2{public}, true, readyAt, readyAt, nil))
	if resumed := project(readyAt); resumed.Incomplete || len(resumed.Items) != 1 {
		t.Fatal("read did not resume after publishing the current generation")
	}
}
