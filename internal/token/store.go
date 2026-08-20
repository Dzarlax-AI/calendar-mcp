package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/oauth2"
)

type FileStore struct {
	path string
	mu   sync.Mutex
}

type Store interface {
	Load() (*oauth2.Token, error)
	Save(*oauth2.Token) error
}

func NewFileStore(dir, provider string) *FileStore {
	return &FileStore{path: filepath.Join(dir, provider+"_token.json")}
}

func (s *FileStore) Load() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pathInfo, err := os.Lstat(s.path)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("token path is not a regular file")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened token file: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("token path changed while opening")
	}
	if openedInfo.Mode().Perm() != 0o600 {
		if err := file.Chmod(0o600); err != nil {
			return nil, fmt.Errorf("secure token permissions: %w", err)
		}
	}
	data, err := io.ReadAll(file)
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
	return TokenSource(s, cfg, initial)
}

func TokenSource(store Store, cfg *oauth2.Config, initial *oauth2.Token) oauth2.TokenSource {
	if initial == nil {
		initial = &oauth2.Token{}
	}
	return &persistingTokenSource{
		store: store,
		src:   cfg.TokenSource(context.Background(), initial),
	}
}

type persistingTokenSource struct {
	store Store
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
