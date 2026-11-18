package storage

import (
	"crypto/sha256"
	"errors"
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
