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
