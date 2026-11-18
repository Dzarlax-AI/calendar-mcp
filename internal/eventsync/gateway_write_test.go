package eventsync

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type syntheticWriteFinder struct {
	receipts map[string]*calendar.EventV2
	lookups  int
}

func (f *syntheticWriteFinder) FindGatewayWriteReceipt(_ context.Context, w storage.GatewayWrite) (*calendar.EventV2, error) {
	f.lookups++
	return f.receipts[w.ID], nil
}

func TestGatewayWriteRecoveryDoesNotRepeatSend(t *testing.T) {
	ctx := t.Context()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "writes.db")
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(err)
	open := func() *storage.Store {
		s, err := storage.Open(ctx, dsn)
		check(err)
		t.Cleanup(func() { s.Close() })
		s.SetArtifactCipher(cipher)
		return s
	}
	db := open()
	check(db.Migrate(ctx))
	now := time.Now().UTC()
	const account, raw = "synthetic", "calendar"
	id := calendar.AccountCalendarID("apple", account, raw)
	check(db.CreateConnection(ctx, storage.Connection{ID: account, Provider: "apple", Status: "connected", EncryptedCredentials: []byte("fixture"),
		CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}))
	proof := storage.GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "apple", Timezone: "UTC"},
		Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "apple"}, Class: storage.GatewayPrivateAccess}
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, account, now, time.Minute)
	check(err)
	check(db.PublishGatewayAuthority(ctx, lease, []storage.GatewayCalendarProof{proof}, now))
	scope := calendar.GatewayWriteScope{Kind: "create", CalendarID: id}
	finder := &syntheticWriteFinder{receipts: map[string]*calendar.EventV2{}}
	start := now.Add(time.Minute)
	event := calendar.EventV2{ID: "recovered", Provider: "apple", CalendarID: id, Visibility: "public", Title: "Synthetic",
		Start: calendar.EventTime{DateTime: start.Format(time.RFC3339), TimeZone: "UTC"},
		End:   calendar.EventTime{DateTime: start.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
	sends := 0
	writeCtx, _ := calendar.EnableGatewayWriteTracking(ctx)
	calendar.DescribeGatewayWrite(writeCtx, nil, scope)
	_, err = calendar.GuardedWrite(writeCtx, db, []string{id}, func() (*calendar.EventV2, error) {
		sends++
		marker := calendar.GatewayWriteDispatchID(writeCtx)
		if marker == "" {
			t.Fatal("send lacked durable marker")
		}
		finder.receipts[marker] = &event
		return nil, errors.New("provider response lost")
	}, func(result *calendar.EventV2) bool { return result != nil })
	if !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) {
		t.Fatalf("unexpected write result: %v", err)
	}
	if _, err := db.BeginGatewayWrite(ctx, []string{id}, scope); err == nil {
		t.Fatal("unknown dispatch did not retain its fence")
	}
	// The worker sees the durable reservation after an independent reopen.
	check(db.Close())
	db = open()
	future := now.Add(2 * time.Minute)
	check(ReconcileGatewayWrite(ctx, db, finder, future))
	check(ReconcileGatewayWrite(ctx, db, finder, future.Add(time.Minute)))
	point, err := db.ReadGatewayEvents(ctx, storage.GatewayEventQuery{CalendarRef: id, EventID: "recovered"}, future, false)
	check(err)
	if sends != 1 || finder.lookups != 1 || len(point.Items) != 1 || point.Items[0].Event.ID != "apple:recovered" {
		t.Fatalf("sends=%d lookups=%d point=%#v", sends, finder.lookups, point.Items)
	}

	// A crash before authorizing dispatch can be closed by the local CAS.
	abandoned, err := db.BeginGatewayWrite(ctx, []string{id}, scope)
	check(err)
	check(ReconcileGatewayWrite(ctx, db, finder, future))
	if err := abandoned.Authorize(ctx); err == nil {
		t.Fatal("abandoned lease allowed a provider dispatch")
	}
	if finder.lookups != 1 {
		t.Fatal("reserved write caused a provider lookup")
	}

	// Persisting the provider result before a crash requires no marker read.
	reservation, err := db.BeginGatewayWrite(ctx, []string{id}, scope)
	check(err)
	check(reservation.Authorize(ctx))
	event.ID = "known"
	check(db.RecordGatewayWriteConfirmation(ctx, reservation.ID(), &event, now))
	check(ReconcileGatewayWrite(ctx, db, finder, future))
	point, err = db.ReadGatewayEvents(ctx, storage.GatewayEventQuery{CalendarRef: id, EventID: "known"}, future, false)
	check(err)
	if len(point.Items) != 1 || finder.lookups != 1 {
		t.Fatal("known receipt was not recovered locally")
	}
}
