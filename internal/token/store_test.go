package token

import (
	"errors"
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
		store: store,
		src: staticTokenSource{token: &oauth2.Token{
			AccessToken: "new-access",
			Expiry:      time.Now().Add(time.Hour),
		}},
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

type staticTokenSource struct {
	token *oauth2.Token
	err   error
}

func (s staticTokenSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.token == nil {
		return nil, errors.New("missing token")
	}
	return s.token, nil
}
