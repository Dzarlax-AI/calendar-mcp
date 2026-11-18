package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type blockingGatewayFinder struct{ started chan struct{} }

func (f *blockingGatewayFinder) FindGatewayWriteReceipt(ctx context.Context, _ storage.GatewayWrite) (*calendar.EventV2, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestGatewayWriteSchedulerExpiresWhileLookupBlocked(t *testing.T) {
	ctx := t.Context()
	db, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetArtifactCipher(cipher)
	now := time.Now().UTC()
	id := calendar.AccountCalendarID("apple", "synthetic", "calendar")
	if err = db.CreateConnection(ctx, storage.Connection{ID: "synthetic", Provider: "apple", Status: "connected", EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, "synthetic", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	proof := storage.GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "apple"}, Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "apple"}, Class: storage.GatewayPrivateAccess}
	if err = db.PublishGatewayAuthority(ctx, lease, []storage.GatewayCalendarProof{proof}, now); err != nil {
		t.Fatal(err)
	}
	scope := calendar.GatewayWriteScope{Kind: "create", CalendarID: id}
	reservation, err := db.BeginGatewayWrite(ctx, []string{id}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = reservation.Authorize(ctx); err != nil {
		t.Fatal(err)
	}
	var offset atomic.Int64
	offset.Store(int64(2 * time.Minute))
	finder := &blockingGatewayFinder{started: make(chan struct{}, 1)}
	schedulerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunGatewayWriteRecovery(schedulerCtx, db, finder, 10*time.Millisecond, func() time.Time { return now.Add(time.Duration(offset.Load())) })
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-finder.started:
	case <-time.After(2 * time.Second):
		t.Fatal("lookup did not start")
	}
	if _, err := db.BeginGatewayWrite(ctx, []string{id}, scope); !errors.Is(err, storage.ErrExclusiveDispatchConflict) {
		t.Fatalf("dispatch fence did not block another reservation before expiry: %v", err)
	}
	offset.Store(int64(6 * time.Minute))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		next, err := db.BeginGatewayWrite(ctx, []string{id}, scope)
		if err == nil { // release the new, never-dispatched reservation through normal recovery
			_ = next
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired fence was not released while lookup remained blocked")
}
