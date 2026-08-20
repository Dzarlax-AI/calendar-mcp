package oauthflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type fakeOAuthProvider struct {
	state, verifier, code string
}

func (p *fakeOAuthProvider) AuthorizationURL(state, challenge string) string {
	p.state = state
	if challenge == "" {
		return "invalid"
	}
	return "https://provider.example/authorize?state=" + state
}

func (p *fakeOAuthProvider) Exchange(_ context.Context, code, verifier string) (*oauth2.Token, error) {
	p.code = code
	p.verifier = verifier
	return &oauth2.Token{RefreshToken: "refresh"}, nil
}

func newOAuthService(t *testing.T) (*Service, *fakeOAuthProvider) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	provider := &fakeOAuthProvider{}
	service := New(store, cipher, map[string]Provider{"google": provider})
	service.now = func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }
	return service, provider
}

func TestOAuthPKCEStateAndSingleUse(t *testing.T) {
	service, provider := newOAuthService(t)
	ctx := context.Background()
	start, err := service.Begin(ctx, "google", "/connections")
	if err != nil {
		t.Fatal(err)
	}
	if start.State == "" || !strings.Contains(start.AuthorizationURL, start.State) || provider.state != start.State {
		t.Fatalf("start = %#v", start)
	}
	token, returnPath, err := service.Complete(ctx, "google", start.State, "authorization-code")
	if err != nil {
		t.Fatal(err)
	}
	if token.RefreshToken != "refresh" || returnPath != "/connections" || provider.verifier == "" || provider.code != "authorization-code" {
		t.Fatalf("token=%#v return=%q verifier=%q", token, returnPath, provider.verifier)
	}
	if _, _, err := service.Complete(ctx, "google", start.State, "authorization-code"); !errors.Is(err, storage.ErrOAuthNotConsumable) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestOAuthRejectsUnsafeReturnPathAndExpiredAttempt(t *testing.T) {
	service, _ := newOAuthService(t)
	ctx := context.Background()
	for _, path := range []string{"https://evil.example", "//evil.example", `/\\evil.example`, "/ok\r\nBad: value"} {
		if _, err := service.Begin(ctx, "google", path); err == nil {
			t.Fatalf("return path %q accepted", path)
		}
	}
	start, err := service.Begin(ctx, "google", "/")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 8, 20, 12, 11, 0, 0, time.UTC) }
	if _, _, err := service.Complete(ctx, "google", start.State, "code"); !errors.Is(err, storage.ErrOAuthNotConsumable) {
		t.Fatalf("expired error = %v", err)
	}
}

func TestOAuthNormalizesEmptyReturnPath(t *testing.T) {
	service, _ := newOAuthService(t)
	start, err := service.Begin(context.Background(), "google", "")
	if err != nil {
		t.Fatal(err)
	}
	_, returnPath, err := service.Complete(context.Background(), "google", start.State, "code")
	if err != nil {
		t.Fatal(err)
	}
	if returnPath != "/connections" {
		t.Fatalf("return path = %q", returnPath)
	}
}

func TestOAuthReconnectCarriesExactConnectionTarget(t *testing.T) {
	service, _ := newOAuthService(t)
	start, err := service.BeginReconnect(context.Background(), "google", "google-account-2", "/connections")
	if err != nil {
		t.Fatal(err)
	}
	completion, err := service.CompleteWithTarget(context.Background(), "google", start.State, "code")
	if err != nil {
		t.Fatal(err)
	}
	if completion.Mode != "reconnect" || completion.ConnectionID != "google-account-2" || completion.ReturnPath != "/connections" {
		t.Fatalf("completion = %#v", completion)
	}
}
