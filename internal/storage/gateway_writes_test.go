package storage

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
)

func TestGatewayWriteReceiptSQLite(t *testing.T) {
	testGatewayWriteReceipt(t, openSQLiteForMigration(t, 29))
}

func testGatewayWriteReceipt(t *testing.T, db *Store) {
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	must(err)
	db.SetArtifactCipher(cipher)
	now := time.Now().UTC()
	const account, raw = "write-receipt", "calendar"
	id := calendar.AccountCalendarID("microsoft", account, raw)
	must(db.CreateConnection(ctx, Connection{ID: account, Provider: "microsoft", Status: "connected", EncryptedCredentials: []byte("fixture"),
		CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, now, time.Minute)
	must(err)
	proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "microsoft", Timezone: "UTC"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "microsoft"}, Class: GatewayPrivateAccess}
	must(db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, now))
	req := GatewayEventQuery{CalendarRef: id, EventID: "event"}
	scope := calendar.GatewayWriteScope{Kind: "create", CalendarID: id}
	reservation, err := db.BeginGatewayWrite(ctx, []string{id}, scope)
	must(err)
	must(reservation.Authorize(ctx))
	if err := db.FinishGatewayWrite(ctx, reservation.ID(), []string{id}, now); !errors.Is(err, ErrExclusiveDispatchConflict) {
		t.Fatalf("unconfirmed write was released: %v", err)
	}
	e := &calendar.EventV2{ID: "event", Provider: "microsoft", CalendarID: id, Title: "synthetic confirmed receipt", Visibility: "normal",
		Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"},
		End:   calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	must(db.RecordGatewayWriteConfirmation(ctx, reservation.ID(), e, now))
	var saved string
	must(db.db.QueryRowContext(ctx, db.query(`SELECT receipt_ciphertext FROM gateway_writes WHERE operation_id=?`), reservation.ID()).Scan(&saved))
	if saved == "" || saved == e.Title {
		t.Fatal("receipt not encrypted")
	}
	must(db.FinishGatewayWrite(ctx, reservation.ID(), []string{id}, now))
	must(db.FinishGatewayWrite(ctx, reservation.ID(), []string{id}, now))
	point, err := db.ReadGatewayEvents(ctx, req, now, false)
	must(err)
	if len(point.Items) != 1 || point.Items[0].Event.Title != e.Title {
		t.Fatalf("invalid point snapshot: %#v", point.Items)
	}
	failedScope := calendar.GatewayWriteScope{Kind: "update", CalendarID: id, EventID: "other"}
	failed, err := db.BeginGatewayWrite(ctx, []string{id}, failedScope)
	must(err)
	must(failed.Authorize(ctx))
	must(failed.Complete(ctx, nil, true))
	point, err = db.ReadGatewayEvents(ctx, req, time.Now(), false)
	must(err)
	if len(point.Items) != 1 || point.Items[0].Event.Title != e.Title {
		t.Fatalf("no-dispatch release discarded the confirmed receipt: %#v", point)
	}
	scope.Kind, scope.EventID, scope.Scope = "delete", "event", calendar.ScopeSingle
	reservation, err = db.BeginGatewayWrite(ctx, []string{id}, scope)
	must(err)
	must(reservation.Authorize(ctx))
	must(reservation.Complete(ctx, &calendar.OperationResult{Status: "completed"}, false))
	point, err = db.ReadGatewayEvents(ctx, req, time.Now(), false)
	must(err)
	if len(point.Items) != 1 || !point.Items[0].Cancelled {
		t.Fatal("confirmed delete did not publish a tombstone")
	}

	// A fresh provider snapshot may disagree with an acknowledged write.
	// Use the receipt only for its bounded lifetime, including range reads.
	grant, err := db.GatewayCalendarGrant(ctx, id, now)
	must(err)
	proof.DetailReceipt = grant.DetailReceipt
	renewAt := now.Add(10 * time.Minute)
	lease, err = db.ClaimGatewayAuthorityCheck(ctx, account, renewAt, time.Minute)
	must(err)
	must(db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, renewAt))
	syncAt := now.Add(12 * time.Minute)
	window := SyncWindow{Start: now.Add(-time.Hour), End: now.Add(2 * time.Hour)}
	must(db.EnsureCalendarSyncStates(ctx, syncAt, window))
	claim, err := db.ClaimDueCalendarSync(ctx, "write-point-fixture", syncAt, syncAt.Add(time.Minute))
	must(err)
	if claim == nil {
		t.Fatal("expected fresh sync claim")
	}
	must(db.ApplyEventSyncPage(ctx, *claim, EventSyncBatch{FullSync: true}, true, syncAt))
	ticket, err := db.CaptureGatewayPublication(ctx, id, syncAt)
	must(err)
	must(db.PublishGatewayEvents(ctx, ticket, []calendar.EventV2{*e}, true, syncAt, syncAt, nil))
	rangeReq := GatewayEventQuery{CalendarIDs: []string{id}, Window: window}
	page, err := db.ReadGatewayEvents(ctx, rangeReq, syncAt, false)
	must(err)
	if !page.Incomplete || len(page.Items) != 0 {
		t.Fatal("fresh write receipt did not fence the conflicting observation")
	}
	readAt := now.Add(16 * time.Minute)
	page, err = db.ReadGatewayEvents(ctx, rangeReq, readAt, false)
	must(err)
	if page.Incomplete || len(page.Items) != 1 || page.Items[0].Cancelled {
		t.Fatalf("expired write receipt hid fresh range: %#v", page)
	}
	point, err = db.ReadGatewayEvents(ctx, req, readAt, false)
	must(err)
	if len(point.Items) != 1 || point.Items[0].Cancelled {
		t.Fatalf("expired write receipt hid fresh point: %#v", point)
	}

	// An unresolved dispatch is closed at its durable deadline. Its
	// reservation can be released without a success receipt or tombstone.
	scope.EventID = "unknown"
	reservation, err = db.BeginGatewayWrite(ctx, []string{id}, scope)
	must(err)
	must(reservation.Authorize(ctx))
	var dueMS int64
	must(db.db.QueryRowContext(ctx, db.query(`SELECT outcome_due_ms FROM gateway_writes WHERE operation_id=?`), reservation.ID()).Scan(&dueMS))
	due := time.UnixMilli(dueMS)
	early := due.Add(-time.Millisecond)
	write, err := db.ClaimGatewayWriteCheck(ctx, early)
	must(err)
	if write == nil || write.ID != reservation.ID() || !write.OutcomeDue.Equal(due) {
		t.Fatalf("unexpected recovery claim: %#v", write)
	}
	if err := db.ExpireGatewayWrite(ctx, *write, early); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("early release got %v", err)
	}
	write, err = db.ClaimGatewayWriteCheck(ctx, due)
	must(err)
	if write == nil || write.ID != reservation.ID() {
		t.Fatalf("write not due at its deadline: %#v", write)
	}
	must(db.ExpireGatewayWrite(ctx, *write, due))
	must(db.ExpireGatewayWrite(ctx, *write, due))
	if err := db.RecordGatewayWriteConfirmation(ctx, reservation.ID(), &calendar.OperationResult{Status: "completed"}, due); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("late response changed unknown outcome: %v", err)
	}
	var phase, receipt string
	var points int
	must(db.db.QueryRowContext(ctx, db.query(`SELECT phase,receipt_ciphertext,
		(SELECT COUNT(*) FROM gateway_write_points p WHERE p.operation_id=w.operation_id)
		FROM gateway_writes w WHERE operation_id=?`), reservation.ID()).Scan(&phase, &receipt, &points))
	if phase != "outcome_unknown" || receipt != "" || points != 0 {
		t.Fatalf("unknown outcome was fabricated: phase=%s hasReceipt=%t points=%d", phase, receipt != "", points)
	}
	if job, err := db.ClaimGatewayWriteCheck(ctx, due.Add(time.Hour)); err != nil || job != nil {
		t.Fatalf("timed-out write is still scheduled: %#v %v", job, err)
	}
}

func TestGatewayMicrosoftSeriesCoverageSQLite(t *testing.T) {
	testGatewayMicrosoftSeriesCoverage(t, openSQLiteForMigration(t, 29))
}

func testGatewayMicrosoftSeriesCoverage(t *testing.T, db *Store) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	const account, raw = "graph", "cal"
	id := calendar.AccountCalendarID("microsoft", account, raw)
	must(db.CreateConnection(ctx, Connection{ID: account, Provider: "microsoft", Status: "connected", EncryptedCredentials: []byte("fixture"),
		CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: day, UpdatedAt: day}))
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, day, time.Minute)
	must(err)
	proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "microsoft"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "microsoft"}, Class: GatewayEventAccess}
	must(db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, day))
	window := SyncWindow{Start: day.Add(-24 * time.Hour), End: day.Add(48 * time.Hour)}
	must(db.EnsureCalendarSyncStates(ctx, day, window))
	claim, err := db.ClaimDueCalendarSync(ctx, "graph", day, day.Add(time.Minute))
	must(err)
	if claim == nil {
		t.Fatal("no fixture sync")
	}
	must(db.ApplyEventSyncPage(ctx, *claim, EventSyncBatch{}, true, day))
	ticket, err := db.CaptureGatewayPublication(ctx, id, day)
	must(err)
	master := calendar.EventV2{ID: "known", InstanceKind: "seriesMaster", Visibility: "normal", Start: calendar.EventTime{Date: "2026-11-18"}, End: calendar.EventTime{Date: "2026-11-19"}}
	unknown := master
	unknown.ID = "unknown"
	cancelled := calendar.EventV2{ID: "canceled", RecurringEventID: "known", Status: "cancelled", Visibility: "normal", OriginalStart: &calendar.EventTime{Date: "2026-11-18"}}
	must(db.PublishGatewayEvents(ctx, ticket, []calendar.EventV2{master, unknown, cancelled}, false, day, day, map[string]string{"known": "Europe/Belgrade"}))
	req := GatewayEventQuery{CalendarRef: id, SeriesID: "known", ShowDeleted: true, Window: SyncWindow{Start: day, End: day.Add(24 * time.Hour)},
		CivilStart: "2026-11-18", CivilEnd: "2026-11-19"}
	out, err := db.ReadGatewayEvents(ctx, req, day, false)
	must(err)
	if out.Incomplete || len(out.Items) != 1 || !out.Items[0].Cancelled {
		t.Fatalf("confirmed series not covered: %#v", out)
	}
	req.SeriesID = "unknown"
	out, err = db.ReadGatewayEvents(ctx, req, day, false)
	must(err)
	if !out.Incomplete || len(out.Items) != 0 {
		t.Fatal("unknown series completeness was inferred")
	}
	req.SeriesID = ""
	out, err = db.ReadGatewayEvents(ctx, req, day, false)
	must(err)
	if !out.Incomplete || len(out.Items) != 1 {
		t.Fatal("series evidence incorrectly proved all Graph deletions")
	}
}

// A timestamp captured before a blocked database operation cannot authorize a
// receipt once the actual database clock has passed the durable deadline.
func TestGatewayConfirmationChecksDatabaseClock(t *testing.T) {
	testGatewayConfirmationChecksDatabaseClock(t, openSQLiteForMigration(t, 29))
}

func testGatewayConfirmationChecksDatabaseClock(t *testing.T, db *Store) {
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetArtifactCipher(cipher)
	now := time.Now()
	id := calendar.AccountCalendarID("apple", "deadline", "cal")
	if err = db.CreateConnection(ctx, Connection{ID: "deadline", Provider: "apple", Status: "connected", EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	reservation, err := db.BeginGatewayWrite(ctx, []string{id}, calendar.GatewayWriteScope{Kind: "create", CalendarID: id})
	// Register a calendar first through the same authority path as production.
	if err == nil {
		t.Fatal("unexpected reservation without calendar")
	}
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, "deadline", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "apple"}, Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "apple"}, Class: GatewayPrivateAccess}
	if err = db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, now); err != nil {
		t.Fatal(err)
	}
	reservation, err = db.BeginGatewayWrite(ctx, []string{id}, calendar.GatewayWriteScope{Kind: "create", CalendarID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err = reservation.Authorize(ctx); err != nil {
		t.Fatal(err)
	}
	delay := 150 * time.Millisecond
	if db.dialect == DialectPostgres {
		delay = 2 * time.Second
	}
	due := time.Now().Add(delay)
	if _, err = db.db.ExecContext(ctx, db.query(`UPDATE gateway_writes SET outcome_due_ms=? WHERE operation_id=?`), due.UnixMilli(), reservation.ID()); err != nil {
		t.Fatal(err)
	}
	// Hold SQLite's single connection or the actual PostgreSQL row lock
	// past the deadline after the caller has captured its timestamp.
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var lockOwner int
	if db.dialect == DialectPostgres {
		if err := tx.QueryRowContext(ctx, db.query(`SELECT pg_backend_pid() FROM gateway_writes WHERE operation_id=? FOR UPDATE`), reservation.ID()).Scan(&lockOwner); err != nil {
			t.Fatal(err)
		}
	}
	result := make(chan error, 1)
	go func() {
		result <- db.RecordGatewayWriteConfirmation(ctx, reservation.ID(), &calendar.OperationResult{Status: "completed"}, now)
	}()
	if lockOwner != 0 {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		timer := time.NewTimer(time.Until(due))
		defer timer.Stop()
		waiting := false
		for !waiting {
			select {
			case err := <-result:
				t.Fatalf("confirmation did not wait on the row lock: %v", err)
			case <-timer.C:
				t.Fatal("confirmation did not reach the PostgreSQL row lock before its deadline")
			case <-ticker.C:
				if err := db.db.QueryRowContext(ctx, db.query(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity
				WHERE ?=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock')`), lockOwner).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if !time.Now().Before(due) {
					t.Fatal("could not establish pre-deadline lock wait")
				}
			}
		}
	}
	time.Sleep(time.Until(due) + 50*time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("stale time accepted after database wait: %v", err)
	}
	if err = db.ExpireOneGatewayWrite(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var phase string
	var resolved bool
	if err := db.db.QueryRowContext(ctx, db.query(`SELECT w.phase,o.resolved_at IS NOT NULL FROM gateway_writes w
	JOIN projection_mutation_operations o ON o.operation_id=w.operation_id WHERE w.operation_id=?`), reservation.ID()).Scan(&phase, &resolved); err != nil {
		t.Fatal(err)
	}
	if phase != "outcome_unknown" || !resolved {
		t.Fatalf("expiry left write phase=%s resolved=%t", phase, resolved)
	}
	err = db.RecordGatewayWriteConfirmation(ctx, reservation.ID(), &calendar.OperationResult{Status: "completed"}, now.Add(-time.Minute))
	if !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("unknown outcome changed: %v", err)
	}
}

func TestGatewayWritePointHonorsGooglePermissionPark(t *testing.T) {
	db := openSQLiteForMigration(t, 29)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	must(err)
	db.SetArtifactCipher(cipher)
	now := time.Now().UTC().Truncate(time.Second)
	const account = "point-permission"
	id := calendar.AccountCalendarID("google", account, "cal")
	must(db.CreateConnection(ctx, Connection{ID: account, Provider: "google", Status: "connected", EncryptedCredentials: []byte("fixture"),
		CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, now, time.Minute)
	must(err)
	proof := GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "google"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "google"}, Class: GatewayPrivateAccess, GoogleRole: "owner"}
	must(db.PublishGatewayAuthority(ctx, lease, []GatewayCalendarProof{proof}, now))
	scope := calendar.GatewayWriteScope{Kind: "create", CalendarID: id}
	reservation, err := db.BeginGatewayWrite(ctx, []string{id}, scope)
	must(err)
	must(reservation.Authorize(ctx))
	event := &calendar.EventV2{ID: "private-point", Visibility: "private", Start: calendar.EventTime{DateTime: now.Format(time.RFC3339), TimeZone: "UTC"},
		End: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	must(reservation.Complete(ctx, event, false))
	at := time.Now().UTC().Add(time.Second)
	query := GatewayEventQuery{CalendarRef: id, EventID: event.ID}
	before, err := db.ReadGatewayEvents(ctx, query, at, false)
	must(err)
	if len(before.Items) != 1 {
		t.Fatal("fixture has no readable write point")
	}
	must(db.EnsureCalendarSyncStates(ctx, at, SyncWindow{Start: at.Add(-time.Hour), End: at.Add(2 * time.Hour)}))
	claim, err := db.ClaimDueCalendarSync(ctx, "permission", at, at.Add(time.Minute))
	must(err)
	if claim == nil {
		t.Fatal("fixture sync not scheduled")
	}
	must(db.ParkCalendarSync(ctx, *claim, "permission", at))
	_, err = db.GatewayCalendarGrant(ctx, id, at)
	must(err) // The park, rather than expired authorization, must fence the point.
	after, err := db.ReadGatewayEvents(ctx, query, at, false)
	must(err)
	if len(after.Items) != 0 || !after.Incomplete {
		t.Fatalf("permission park exposed a private write point: %#v", after)
	}
}

func TestGatewayMoveCannotAuthorizeDisabledDestination(t *testing.T) {
	db := openSQLiteForMigration(t, 29)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx))
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	must(err)
	db.SetArtifactCipher(cipher)
	now := time.Now().UTC().Truncate(time.Second)
	const account = "move-authority"
	src := calendar.AccountCalendarID("microsoft", account, "src")
	dst := calendar.AccountCalendarID("microsoft", account, "dst")
	must(db.CreateConnection(ctx, Connection{ID: account, Provider: "microsoft", Status: "connected", EncryptedCredentials: []byte("fixture"),
		CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, now, time.Minute)
	must(err)
	ids := []string{src, dst}
	var proofs []GatewayCalendarProof
	for _, id := range ids {
		proofs = append(proofs, GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "microsoft"},
			Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "microsoft"}, Class: GatewayPrivateAccess})
	}
	must(db.PublishGatewayAuthority(ctx, lease, proofs, now))
	scope := calendar.GatewayWriteScope{Kind: "move", CalendarID: src, DestinationCalendarID: dst}
	reservation, err := db.BeginGatewayWrite(ctx, ids, scope)
	must(err)
	must(db.SetCalendarEnabled(ctx, dst, false, now))
	if err := reservation.Authorize(ctx); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("move admitted an inactive destination: %v", err)
	}
	if !reservation.OutcomeDeadline().IsZero() {
		t.Fatal("failed authorization set an outcome deadline")
	}
}
