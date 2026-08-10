package token

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/oauth2"
)

type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(dir, provider string) *FileStore {
	return &FileStore{path: filepath.Join(dir, provider+"_token.json")}
}

func (s *FileStore) Load() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func (s *FileStore) Save(tok *oauth2.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary token file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set token permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace token: %w", err)
	}
	removeTemp = false
	return nil
}

// TokenSource returns an oauth2.TokenSource that persists refreshed tokens to disk.
func (s *FileStore) TokenSource(cfg *oauth2.Config, initial *oauth2.Token) oauth2.TokenSource {
	return &persistingTokenSource{
		store: s,
		src:   cfg.TokenSource(context.Background(), initial),
	}
}

type persistingTokenSource struct {
	store *FileStore
	src   oauth2.TokenSource
	mu    sync.Mutex
	last  *oauth2.Token
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	if p.last == nil || tokenChanged(tok, p.last) {
		if err := p.store.Save(tok); err != nil {
			return nil, fmt.Errorf("persist refreshed token: %w", err)
		}
		p.last = tok
	}
	return tok, nil
}

func tokenChanged(current, previous *oauth2.Token) bool {
	return current.AccessToken != previous.AccessToken ||
		current.RefreshToken != previous.RefreshToken ||
		current.TokenType != previous.TokenType ||
		!current.Expiry.Equal(previous.Expiry)
}
