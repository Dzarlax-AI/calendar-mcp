package token

import (
	"encoding/json"
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
	return os.WriteFile(s.path, data, 0600)
}

// TokenSource returns an oauth2.TokenSource that persists refreshed tokens to disk.
func (s *FileStore) TokenSource(cfg *oauth2.Config, initial *oauth2.Token) oauth2.TokenSource {
	return &persistingTokenSource{
		store: s,
		src:   cfg.TokenSource(nil, initial),
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
	if p.last == nil || tok.AccessToken != p.last.AccessToken {
		_ = p.store.Save(tok)
		p.last = tok
	}
	return tok, nil
}
