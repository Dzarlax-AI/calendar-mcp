package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	store          Store
	src            oauth2.TokenSource
	mu             sync.Mutex
	last           *oauth2.Token
	expectedScopes []string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, _, _, err := p.TokenWithBinding()
	return tok, err
}

// TokenWithBinding serializes token selection/persistence with its exact
// credential snapshot. On refresh failure it returns the attempted binding.
func (p *persistingTokenSource) TokenWithBinding() (*oauth2.Token, string, [32]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var revision string
	var digest [32]byte
	if bound, ok := p.store.(interface{ CredentialBinding() (string, [32]byte) }); ok {
		revision, digest = bound.CredentialBinding()
	}
	tok, err := p.src.Token()
	if err != nil {
		return nil, revision, digest, err
	}
	if p.expectedScopes != nil {
		if observed := tok.Extra("scope"); observed != nil {
			value, ok := observed.(string)
			if !ok || !hasRequiredScopes(strings.Fields(value), p.expectedScopes) {
				return nil, revision, digest, ErrAuthorizationScopeChanged
			}
		}
	}
	if p.last == nil || tokenChanged(tok, p.last) {
		if err := p.store.Save(tok); err != nil {
			return nil, revision, digest, fmt.Errorf("persist refreshed token: %w", err)
		}
		p.last = tok
	}
	if bound, ok := p.store.(interface{ CredentialBinding() (string, [32]byte) }); ok {
		revision, digest = bound.CredentialBinding()
	}
	return tok, revision, digest, nil
}

func tokenChanged(current, previous *oauth2.Token) bool {
	return current.AccessToken != previous.AccessToken ||
		current.RefreshToken != previous.RefreshToken ||
		current.TokenType != previous.TokenType ||
		!current.Expiry.Equal(previous.Expiry)
}

// RequestBinding is private to a single HTTP call and never contains tokens.
type RequestBinding struct {
	Revision       string
	Digest         [32]byte
	Set            bool
	AuthorityError error
}
type requestBindingKey struct{}

func WithRequestBinding(ctx context.Context, b *RequestBinding) context.Context {
	return context.WithValue(ctx, requestBindingKey{}, b)
}
func RequestBindingFromContext(ctx context.Context) *RequestBinding {
	b, _ := ctx.Value(requestBindingKey{}).(*RequestBinding)
	return b
}

// BoundTransport associates the token selected for this request with the
// revision/ciphertext binding selected under the same token-source mutex.
type BoundTransport struct {
	Source interface {
		TokenWithBinding() (*oauth2.Token, string, [32]byte, error)
	}
	Base http.RoundTripper
}

func (t *BoundTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, revision, digest, err := t.Source.TokenWithBinding()
	if b := RequestBindingFromContext(r.Context()); b != nil {
		b.Revision, b.Digest, b.Set = revision, digest, true
	}
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	tok.SetAuthHeader(clone)
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// SnapshotTransport constructs a request-bound source while preserving the
// same persistence behavior as TokenSource.
func SnapshotTransport(store Store, cfg *oauth2.Config, initial *oauth2.Token) http.RoundTripper {
	source := TokenSource(store, cfg, initial).(*persistingTokenSource)
	source.expectedScopes = append([]string(nil), cfg.Scopes...)
	return &BoundTransport{Source: source}
}

var ErrAuthorizationScopeChanged = errors.New("OAuth authorization scope changed")

// Explicit scopes must retain all configured permissions; Google full-calendar
// scope also covers its narrower CalendarList and Events scopes. Additional
// granted scopes are not evidence of a change from an unknown legacy baseline.
func hasRequiredScopes(observed, required []string) bool {
	present := make(map[string]bool, len(observed))
	for _, v := range observed {
		present[v] = true
	}
	for _, need := range required {
		if present[need] {
			continue
		}
		if present["https://www.googleapis.com/auth/calendar"] && strings.HasPrefix(need, "https://www.googleapis.com/auth/calendar.") {
			continue
		}
		return false
	}
	return true
}
