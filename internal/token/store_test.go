package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestFileStoreSaveCreatesDirectoryAndUsesPrivatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "tokens")
	store := NewFileStore(dir, "google")
	token := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}

	if err := store.Save(token); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "google_token.json"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o, want 600", got)
	}
}

func TestFileStoreLoadRepairsExistingPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "google_token.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"access"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(dir, "google")
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o, want 600", got)
	}
}

func TestFileStoreLoadRejectsSymbolicLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"access_token":"target"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "google_token.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	store := NewFileStore(dir, "google")
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() accepted a symbolic-link token path")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("target permissions = %o, want unchanged 644", got)
	}
}

func TestPersistingTokenSourceReturnsSaveFailure(t *testing.T) {
	root := t.TempDir()
	blockingFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(filepath.Join(blockingFile, "tokens"), "google")
	source := &persistingTokenSource{
		store:  store,
		config: &oauth2.Config{},
		current: &oauth2.Token{
			AccessToken: "new-access",
			Expiry:      time.Now().Add(time.Hour),
		},
	}

	if _, err := source.Token(); err == nil {
		t.Fatal("Token() returned nil error for token persistence failure")
	}
}

func TestTokenSourceAcceptsMissingInitialToken(t *testing.T) {
	store := NewFileStore(t.TempDir(), "google")
	source := TokenSource(store, &oauth2.Config{}, nil)
	if _, err := source.Token(); err == nil {
		t.Fatal("Token() succeeded without any OAuth credentials")
	}
}

func TestSnapshotTransportCancelsStalledTokenRefresh(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &http.Client{Transport: SnapshotTransport(NewFileStore(t.TempDir(), "google"), &oauth2.Config{
		Endpoint: oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams},
	}, &oauth2.Token{RefreshToken: "fixture-refresh"})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unreachable.invalid/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := client.Do(req)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancelled refresh, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh remained blocked after request cancellation")
	}
}
