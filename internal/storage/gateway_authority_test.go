package storage

import (
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestGatewayAuthoritySQLiteMigration29(t *testing.T) {
	testGatewayAuthority(t, openSQLiteForMigration(t, 29))
}

func testGatewayAuthority(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	account := Connection{ID: "synthetic", Provider: "apple", Status: "connected", EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConnection(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterDiscoveredCalendar(ctx, "apple@synthetic", calendar.Calendar{ID: "/fixture/", Name: "Synthetic"}); err != nil {
		t.Fatal(err)
	}
	cals, err := store.ListCalendars(ctx, account.ID)
	if err != nil || len(cals) != 1 {
		t.Fatalf("fixture catalog: %v, %v", cals, err)
	}
	id := cals[0].ID
	proof := GatewayCalendarProof{
		Metadata:      calendar.Calendar{ID: id, Provider: account.Provider, Name: "Synthetic"},
		Capabilities:  calendar.CalendarCapabilities{CalendarID: id, Provider: account.Provider},
		DetailReceipt: cals[0].DetailReceipt,
		Class:         GatewayPrivateAccess,
	}
	checkClosed := func(at time.Time) {
		t.Helper()
		if _, err := store.GatewayCalendarGrant(ctx, id, at); !errors.Is(err, ErrAuthorityChanged) {
			t.Fatalf("expected unavailable grant: %v", err)
		}
	}
	claim := func(at time.Time) GatewayAuthorityLease {
		t.Helper()
		ids, err := store.DueGatewayAuthorityChecks(ctx, at, 8)
		if err != nil || len(ids) != 1 || ids[0] != account.ID {
			t.Fatalf("due accounts: %v, %v", ids, err)
		}
		lease, err := store.ClaimGatewayAuthorityCheck(ctx, account.ID, at, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimGatewayAuthorityCheck(ctx, account.ID, at, time.Minute); !errors.Is(err, ErrAuthorityChanged) {
			t.Fatalf("parallel claim: %v", err)
		}
		return lease
	}
	publish := func(at time.Time) GatewayCalendarGrant {
		t.Helper()
		lease := claim(at)
		if err := store.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, at.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := store.PublishGatewayAuthority(ctx, lease, nil, at.Add(2*time.Second)); !errors.Is(err, ErrAuthorityChanged) {
			t.Fatalf("replayed claim: %v", err)
		}
		grants, err := store.GatewayCalendarInventory(ctx, at.Add(time.Second))
		if err != nil || len(grants) != 1 || grants[0].Metadata.ID != id {
			t.Fatalf("inventory: %v, %v", grants, err)
		}
		if !grants[0].VerifiedUntil.Equal(at.Add(GatewayAuthorityLifetime)) {
			t.Fatalf("verified until: %v", grants[0].VerifiedUntil)
		}
		return grants[0]
	}
	checkClosed(now)
	if _, err := store.GatewayCalendarInventory(ctx, now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("legacy catalog published before a check: %v", err)
	}
	first := publish(now)
	same := publish(now.Add(5 * time.Minute))
	if first.Revision != same.Revision {
		t.Fatal("timely identical renewal changed the grant revision")
	}
	stalled := claim(now.Add(10 * time.Minute))
	afterLease := now.Add(11 * time.Minute)
	if err := store.PublishGatewayAuthority(ctx, stalled, nil, afterLease); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("expired check could replace the inventory: %v", err)
	}
	if err := store.FailGatewayAuthorityCheck(ctx, stalled, &GatewayAuthorityDenial{CredentialDigest: sha256.Sum256([]byte("fixture"))}, afterLease); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("expired check could revoke current authority: %v", err)
	}
	stillValid, err := store.GatewayCalendarGrant(ctx, id, afterLease)
	if err != nil || stillValid.Revision != same.Revision || !stillValid.VerifiedUntil.Equal(same.VerifiedUntil) {
		t.Fatalf("expired check changed the active grant: %v", err)
	}
	failed := claim(afterLease)
	if err := store.FailGatewayAuthorityCheck(ctx, failed, nil, afterLease); err != nil {
		t.Fatal(err)
	}
	stillValid, err = store.GatewayCalendarGrant(ctx, id, afterLease.Add(time.Second))
	if err != nil || stillValid.Revision != same.Revision || !stillValid.VerifiedUntil.Equal(same.VerifiedUntil) {
		t.Fatalf("transient failure changed the active grant: %v", err)
	}
	checkClosed(same.VerifiedUntil)
	reopened := publish(now.Add(21 * time.Minute))
	if reopened.Revision == same.Revision || reopened.Class != GatewayPrivateAccess {
		t.Fatal("reopening the expired grant reused the old proof")
	}
	proof.Class = GatewayEventAccess
	next := publish(now.Add(26 * time.Minute))
	if next.Revision == reopened.Revision || next.Class != GatewayEventAccess {
		t.Fatal("a confirmed downgrade reused the wider proof")
	}
	revocation := claim(now.Add(31 * time.Minute))
	if err := store.FailGatewayAuthorityCheck(ctx, revocation, &GatewayAuthorityDenial{CredentialDigest: sha256.Sum256([]byte("fixture"))}, now.Add(31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	checkClosed(now.Add(31 * time.Minute))
	empty := claim(now.Add(32 * time.Minute))
	if err := store.PublishGatewayAuthority(ctx, empty, nil, now.Add(32*time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
	grants, err := store.GatewayCalendarInventory(ctx, now.Add(33*time.Minute))
	if err != nil || grants == nil || len(grants) != 0 {
		t.Fatalf("confirmed empty inventory: %v, %v", grants, err)
	}
	late := claim(now.Add(37 * time.Minute))
	if err := store.PublishGatewayAuthority(ctx, late, []GatewayCalendarProof{proof}, now.Add(38*time.Minute)); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("expired result published: %v", err)
	}
	reconnect := claim(now.Add(38 * time.Minute))
	if _, err := store.BeginAuthorization(ctx, account.ID, []byte("new fixture"), now.Add(38*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishGatewayAuthority(ctx, reconnect, []GatewayCalendarProof{proof}, now.Add(38*time.Minute+time.Second)); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("previous connection published after replacement: %v", err)
	}
}

func TestGatewayGraceSQLite(t *testing.T) {
	testGatewayGrace(t, openSQLiteForMigration(t, 29))
}

func testGatewayGrace(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("apple", "grace", "calendar")
	credential := []byte("synthetic-grace")
	if err := store.CreateConnection(ctx, Connection{ID: "grace", Provider: "apple", Status: "connected",
		EncryptedCredentials: credential, CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "apple"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "apple"}, Class: GatewayEventAccess}
	check := func(at time.Time, timeout time.Duration) GatewayAuthorityLease {
		t.Helper()
		lease, err := store.ClaimGatewayAuthorityCheck(ctx, "grace", at, timeout)
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	okAt := func(at time.Time) GatewayCalendarGrant {
		t.Helper()
		grant, err := store.GatewayCalendarGrant(ctx, id, at)
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	closedAt := func(at time.Time) {
		t.Helper()
		if _, err := store.GatewayCalendarGrant(ctx, id, at); !errors.Is(err, ErrAuthorityChanged) {
			t.Fatalf("at %s, proof should be closed: %v", at, err)
		}
	}
	first := check(now, time.Minute)
	if err := store.PublishGatewayAuthority(ctx, first, []GatewayCalendarProof{proof}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	current := okAt(now.Add(time.Second))
	proof.DetailReceipt = current.DetailReceipt
	expires := now.Add(GatewayAuthorityLifetime)
	lease := check(expires.Add(-time.Second), 5*time.Minute)
	beforeCap := expires.Add(59 * time.Second)
	during := okAt(beforeCap)
	if during.Revision != current.Revision || !during.VerifiedUntil.Equal(expires.Add(time.Minute)) || !during.HardVerifiedUntil.Equal(expires) {
		t.Fatal("grace changed the proof or exceeded its bound")
	}
	closedAt(expires.Add(time.Minute))
	deniedAt := expires.Add(59 * time.Second)
	denial := &GatewayAuthorityDenial{CredentialDigest: sha256.Sum256(credential)}
	if err := store.FailGatewayAuthorityCheck(ctx, lease, denial, deniedAt); err != nil {
		t.Fatal(err)
	}
	closedAt(deniedAt)

	late := check(expires.Add(2*time.Minute), time.Minute)
	closedAt(expires.Add(2*time.Minute + time.Second))
	if err := store.PublishGatewayAuthority(ctx, late, []GatewayCalendarProof{proof}, expires.Add(2*time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
	restored := okAt(expires.Add(2*time.Minute + time.Second))
	if restored.Revision == current.Revision {
		t.Fatal("recovery revived a revoked grant revision")
	}
}

func TestGatewayPartialEvidenceRetainsOnlyOriginalSourceDeadline(t *testing.T) {
	s := openSQLiteForMigration(t, 29)
	ctx := t.Context()
	now := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateConnection(ctx, Connection{ID: "partial", Provider: "microsoft", Status: "connected", EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	proof := func(raw string) GatewayCalendarProof {
		id := calendar.AccountCalendarID("microsoft", "partial", raw)
		return GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "microsoft"}, Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "microsoft"}, Class: GatewayEventAccess}
	}
	a, b := proof("a"), proof("b")
	lease, err := s.ClaimGatewayAuthorityCheck(ctx, "partial", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{a, b}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*GatewayCalendarProof{&a, &b} {
		g, e := s.GatewayCalendarGrant(ctx, p.Metadata.ID, now.Add(time.Second))
		if e != nil {
			t.Fatal(e)
		}
		p.DetailReceipt = g.DetailReceipt
	}
	later := now.Add(10 * time.Minute)
	lease, err = s.ClaimGatewayAuthorityCheck(ctx, "partial", later, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b.Unknown = true
	if err = s.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{a, b}, later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GatewayCalendarInventory(ctx, later.Add(time.Second)); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("partial inventory: %v", err)
	}
	if _, err = s.GatewayCalendarGrant(ctx, b.Metadata.ID, now.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GatewayCalendarGrant(ctx, b.Metadata.ID, now.Add(15*time.Minute)); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("unknown source extended: %v", err)
	}
	if _, err = s.GatewayCalendarGrant(ctx, a.Metadata.ID, now.Add(16*time.Minute)); err != nil {
		t.Fatalf("healthy sibling expired: %v", err)
	}
}

func TestGatewayAliasesRemainAmbiguousAfterProofExpiry(t *testing.T) {
	db := openSQLiteForMigration(t, 29)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, account := range []string{"first", "second"} {
		at := now.Add(time.Duration(i) * time.Minute)
		id := calendar.AccountCalendarID("google", account, "owner@example.invalid")
		must(db.CreateConnection(ctx, Connection{ID: account, Provider: "google", Status: "connected", EncryptedCredentials: []byte("fixture"),
			CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: at, UpdatedAt: at}))
		receipt := seedOrdinaryGoogleReceipt(t, db, account, id, "owner@example.invalid", at, true)
		lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, at, time.Minute)
		must(err)
		proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "google", Primary: true},
			Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "google"}, Class: GatewayPrivateAccess, GoogleRole: "owner", DetailReceipt: receipt}
		must(db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, at))
	}
	checkAt := now.Add(GatewayAuthorityLifetime + 30*time.Second)
	for _, alias := range []string{"google:primary", "google:owner@example.invalid"} {
		if _, err := db.GatewayCalendarGrantByReference(ctx, alias, checkAt); !errors.Is(err, ErrAuthorityChanged) {
			t.Fatalf("unavailable peer made alias %q unique: %v", alias, err)
		}
		out, err := db.ReadGatewayEvents(ctx, GatewayEventQuery{CalendarRef: alias}, checkAt, false)
		must(err)
		if !out.Incomplete || len(out.Sources) != 0 {
			t.Fatalf("event read silently selected an alternate account: %#v", out)
		}
	}
	id := calendar.AccountCalendarID("google", "second", "owner@example.invalid")
	grant, err := db.GatewayCalendarGrantByReference(ctx, id, checkAt)
	must(err)
	if grant.Metadata.ID != id {
		t.Fatalf("canonical reference resolved to %s", grant.Metadata.ID)
	}
}

func TestGatewayWriteCapabilityUpdatesWithoutReadBindingChange(t *testing.T) {
	db := openSQLiteForMigration(t, 29)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	now := time.Now().UTC().Truncate(time.Millisecond)
	const account = "writable"
	id := calendar.AccountCalendarID("microsoft", account, "calendar")
	must(db.CreateConnection(ctx, Connection{ID: account, Provider: "microsoft", Status: "connected", EncryptedCredentials: []byte("fixture"),
		CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
	proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "microsoft"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "microsoft"}, Class: GatewayEventAccess}
	for i, canEdit := range []bool{false, true, false} {
		at := now.Add(time.Duration(i) * GatewayAuthorityInterval)
		lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, at, time.Minute)
		must(err)
		proof.Capabilities.Operations.Update = canEdit
		must(db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, at))
		result, err := db.ListCalendars(ctx, account)
		must(err)
		if len(result) != 1 || result[0].CanWrite != canEdit || result[0].DetailReceipt == "" ||
			i > 0 && result[0].DetailReceipt != proof.DetailReceipt {
			t.Fatalf("edit permission and read receipt diverged: canEdit=%t calendars=%#v", canEdit, result)
		}
		proof.DetailReceipt = result[0].DetailReceipt
	}
}

func seedOrdinaryGoogleReceipt(t *testing.T, db *Store, account, id, raw string, at time.Time, primary bool) string {
	t.Helper()
	ctx := t.Context()
	snapshot, err := db.BeginAuthorization(ctx, account, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteGoogleAuthorization(ctx, snapshot, []DiscoveredDetailCalendar{{
		Calendar:   Calendar{ID: id, ConnectionID: account, ProviderCalendarID: raw, Name: "Synthetic", GooglePrimary: primary},
		AccessRole: "owner",
	}}, at); err != nil {
		t.Fatal(err)
	}
	calendars, err := db.ListCalendars(ctx, account)
	if err != nil || len(calendars) != 1 || DetailReceiptRole(calendars[0].DetailReceipt) != "owner" {
		t.Fatalf("ordinary authorization fixture failed: %v", err)
	}
	return calendars[0].DetailReceipt
}

func TestGatewayPreparationFenceSQLite(t *testing.T) {
	testGatewayPreparationFence(t, openSQLiteForMigration(t, 29))
}

func testGatewayPreparationFence(t *testing.T, db *Store) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	now := time.Now().UTC().Truncate(time.Second)
	create := func(id, provider string) {
		must(db.CreateConnection(ctx, Connection{ID: id, Provider: provider, Status: "connected",
			EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
	}
	create("legacy-apple", "apple")
	create("live-google", "google")
	appleID := calendar.AccountCalendarID("apple", "legacy-apple", "/old/")
	googleID := calendar.AccountCalendarID("google", "live-google", "normal")
	must(db.UpsertCalendar(ctx, Calendar{ID: appleID, ConnectionID: "legacy-apple", ProviderCalendarID: "/old/",
		Name: "Old", Timezone: "UTC", CanRead: true, DiscoveredAt: now}))
	_, err := db.db.ExecContext(ctx, db.query(`UPDATE connections SET authorization_revision='' WHERE id=?`), "legacy-apple")
	must(err)
	_, err = db.db.ExecContext(ctx, db.query(`UPDATE calendars SET detail_receipt='' WHERE id=?`), appleID)
	must(err)
	receipt := seedOrdinaryGoogleReceipt(t, db, "live-google", googleID, "normal", now, false)
	day := now.Truncate(24 * time.Hour)
	window := SyncWindow{Start: day.AddDate(0, 0, -1), End: day.AddDate(0, 0, 2)}
	must(db.EnsureCalendarSyncStates(ctx, now, window))
	appleSync, err := db.ClaimDueCalendarSync(ctx, "apple-before-warmup", now, now.Add(time.Minute))
	must(err)
	if appleSync == nil || appleSync.CalendarID != appleID {
		t.Fatal("could not seed the old Apple cache")
	}
	next := now.Add(time.Hour)
	must(db.ApplyEventSyncPage(ctx, *appleSync, EventSyncBatch{
		FullSync: true, NextCursor: "legacy-token", NextSyncAt: &next,
		Upserts: []CachedEventUpsert{{Event: calendar.EventV2{
			ID: "legacy-event", CalendarID: appleID, Provider: "apple", Title: "Legacy event", Status: "confirmed", InstanceKind: "single",
			Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"},
			End:   calendar.EventTime{DateTime: now.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"},
		}}},
	}, true, now))
	assertAppleCache := func() {
		t.Helper()
		var cursor, status string
		var generation int64
		checkpoint := db.scanTime()
		must(db.db.QueryRowContext(ctx, db.query(db.timeProjection(`SELECT generation,cursor,status,next_sync_at FROM calendar_sync_state WHERE calendar_id=?`, "next_sync_at")), appleID).Scan(&generation, &cursor, &status, &checkpoint))
		if generation != appleSync.Generation || cursor != "legacy-token" || status != "ready" || !checkpoint.Time.Equal(next) {
			t.Fatal("warmup changed the ordinary Apple checkpoint")
		}
		page, err := db.ReadCachedPage(ctx, CachedPageRequest{CalendarIDs: []string{appleID},
			Start: day, End: day.AddDate(0, 0, 1), StartDate: day.Format(time.DateOnly),
			EndDate: day.AddDate(0, 0, 1).Format(time.DateOnly), PageSize: 5, WithCoverage: true, TargetWindow: window}, nil)
		must(err)
		if page.Partial || len(page.Rows) != 1 || page.Rows[0].Summary.EventID != "legacy-event" {
			t.Fatalf("warmup changed the ordinary Apple cached read: partial=%v rows=%v sources=%+v", page.Partial, len(page.Rows), page.Sources)
		}
	}
	assertAppleCache()
	due, err := db.DueGatewayAuthorityChecks(ctx, now, 16)
	must(err)
	if !slices.Contains(due, "legacy-apple") {
		t.Fatal("old Apple connection was not scheduled")
	}
	appleLease, err := db.ClaimGatewayAuthorityCheck(ctx, "legacy-apple", now, time.Minute)
	must(err)
	if appleLease.AuthorizationRevision == "" {
		t.Fatal("old Apple connection has no fence")
	}
	if _, err := db.GatewayCalendarGrant(ctx, appleID, now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("claim opened Apple before live proof: %v", err)
	}
	apple, err := db.ListCalendars(ctx, "legacy-apple")
	must(err)
	if len(apple) != 1 || !apple[0].CanRead || apple[0].DetailReceipt != "" {
		t.Fatal("Apple claim revoked the ordinary calendar")
	}
	must(db.PublishGatewayAuthority(ctx, appleLease, []GatewayCalendarProof{{
		Metadata:     calendar.Calendar{ID: appleID, Provider: "apple"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: appleID, Provider: "apple"},
		Class:        GatewayPrivateAccess, DetailReceipt: apple[0].DetailReceipt,
	}}, now))
	_, err = db.GatewayCalendarGrant(ctx, appleID, now)
	must(err)
	assertAppleCache()

	checkOrdinaryGoogle := func() {
		t.Helper()
		cals, err := db.ListCalendars(ctx, "live-google")
		must(err)
		if len(cals) != 1 || !cals[0].CanRead || cals[0].DetailReceipt != receipt {
			t.Fatal("strict preparation changed ordinary Google authorization")
		}
		var generation int64
		var status string
		must(db.db.QueryRowContext(ctx, db.query(`SELECT generation,status FROM calendar_sync_state WHERE calendar_id=?`), googleID).Scan(&generation, &status))
		if generation != 0 || status != "pending" {
			t.Fatalf("strict preparation reset the ordinary sync: generation=%d status=%q", generation, status)
		}
	}
	googleEvidence := func(role string, class GatewayAccessClass) []GatewayCalendarProof {
		return []GatewayCalendarProof{{
			Metadata:     calendar.Calendar{ID: googleID, Provider: "google"},
			Capabilities: calendar.CalendarCapabilities{CalendarID: googleID, Provider: "google"},
			Class:        class, GoogleRole: role, DetailReceipt: receipt,
		}}
	}
	publishGoogle := func(at time.Time, role string, class GatewayAccessClass) GatewayCalendarGrant {
		t.Helper()
		lease, err := db.ClaimGatewayAuthorityCheck(ctx, "live-google", at, time.Minute)
		must(err)
		must(db.PublishGatewayAuthority(ctx, lease, googleEvidence(role, class), at))
		grant, err := db.GatewayCalendarGrant(ctx, googleID, at)
		must(err)
		return grant
	}
	first := publishGoogle(now, "owner", GatewayPrivateAccess)
	if first.Class != GatewayPrivateAccess {
		t.Fatal("matching Google role could not open the strict grant")
	}
	bad := sha256.Sum256([]byte("stale"))
	if err := db.InvalidateGatewayGooglePublication(ctx, "live-google", first, first.ConnectionRevision, bad); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("old credentials affected a strict grant: %v", err)
	}
	renewAt := now.Add(GatewayAuthorityInterval)
	inFlight, err := db.ClaimGatewayAuthorityCheck(ctx, "live-google", renewAt, time.Minute)
	must(err)
	digest := sha256.Sum256([]byte("synthetic"))
	must(db.InvalidateGatewayGooglePublication(ctx, "live-google", first, first.ConnectionRevision, digest))
	must(db.PublishGatewayAuthority(ctx, inFlight, googleEvidence("owner", GatewayPrivateAccess), renewAt.Add(time.Second)))
	if _, err := db.GatewayCalendarGrant(ctx, googleID, renewAt.Add(time.Second)); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("older account check reopened the strict grant after denial: %v", err)
	}
	if _, err := db.GatewayCalendarGrant(ctx, googleID, now); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("strict access survived confirmed denial: %v", err)
	}
	checkOrdinaryGoogle()
	lower := publishGoogle(now.Add(2*GatewayAuthorityInterval), "reader", GatewayEventAccess)
	if lower.Class != GatewayMetadataOnly {
		t.Fatalf("strict publication reused a conflicting permission: %q", lower.Class)
	}
	checkOrdinaryGoogle()
	again := publishGoogle(now.Add(3*GatewayAuthorityInterval), "owner", GatewayPrivateAccess)
	if again.Class != GatewayPrivateAccess || again.Revision == first.Revision {
		t.Fatal("fresh strict confirmation failed to reopen Google")
	}
	checkOrdinaryGoogle()

	// CalendarList may extend the same permission while an Events.List
	// request is still in flight. The later denial must close that grant.
	renewedAt := now.Add(4 * GatewayAuthorityInterval)
	renewed := publishGoogle(renewedAt, "owner", GatewayPrivateAccess)
	if renewed.Revision != again.Revision || renewed.DetailReceipt != again.DetailReceipt ||
		!renewed.HardVerifiedUntil.After(again.HardVerifiedUntil) {
		t.Fatal("fixture did not renew the same grant")
	}
	must(db.InvalidateGatewayGooglePublication(ctx, "live-google", again, again.ConnectionRevision, digest))
	if _, err := db.GatewayCalendarGrant(ctx, googleID, renewedAt); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("confirmed denial did not close the renewed grant: %v", err)
	}
	reopenedAt := now.Add(5 * GatewayAuthorityInterval)
	reopened := publishGoogle(reopenedAt, "owner", GatewayPrivateAccess)
	if err := db.InvalidateGatewayGooglePublication(ctx, "live-google", again, again.ConnectionRevision, digest); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("denial from an older grant affected a replacement: %v", err)
	}
	current, err := db.GatewayCalendarGrant(ctx, googleID, reopenedAt)
	must(err)
	if current.Revision != reopened.Revision || current.Class != GatewayPrivateAccess {
		t.Fatal("old denial changed the replacement grant")
	}
	checkOrdinaryGoogle()
}
