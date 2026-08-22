package oauthflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

const attemptLifetime = 10 * time.Minute

type Provider interface {
	AuthorizationURL(state, challenge string) string
	Exchange(context.Context, string, string) (*oauth2.Token, error)
}

type ConfigProvider struct {
	Config               *oauth2.Config
	AuthorizationOptions []oauth2.AuthCodeOption
}

func (p ConfigProvider) AuthorizationURL(state, challenge string) string {
	options := []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("code_challenge", challenge), oauth2.SetAuthURLParam("code_challenge_method", "S256")}
	options = append(options, p.AuthorizationOptions...)
	return p.Config.AuthCodeURL(state, options...)
}

func (p ConfigProvider) Exchange(ctx context.Context, code, verifier string) (*oauth2.Token, error) {
	return p.Config.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
}

type Service struct {
	store     *storage.Store
	cipher    *credentials.Cipher
	providers map[string]Provider
	now       func() time.Time
}

type Start struct {
	AuthorizationURL string
	State            string
}

type Completion struct {
	Token        *oauth2.Token
	ReturnPath   string
	ConnectionID string
	Mode         string
}

func New(store *storage.Store, cipher *credentials.Cipher, providers map[string]Provider) *Service {
	return &Service{store: store, cipher: cipher, providers: providers, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Begin(ctx context.Context, provider, returnPath string) (Start, error) {
	return s.begin(ctx, provider, returnPath, "", "add")
}

func (s *Service) BeginReconnect(ctx context.Context, provider, connectionID, returnPath string) (Start, error) {
	if connectionID == "" {
		return Start{}, errors.New("connection id is required for reconnect")
	}
	return s.begin(ctx, provider, returnPath, connectionID, "reconnect")
}

func (s *Service) begin(ctx context.Context, provider, returnPath, connectionID, mode string) (Start, error) {
	client, ok := s.providers[provider]
	if !ok {
		return Start{}, fmt.Errorf("unsupported OAuth provider %q", provider)
	}
	if returnPath == "" {
		returnPath = "/connections"
	}
	if !validReturnPath(returnPath) {
		return Start{}, errors.New("invalid OAuth return path")
	}
	state, err := randomURLToken(32)
	if err != nil {
		return Start{}, err
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		return Start{}, err
	}
	hash := stateHash(state)
	encrypted, err := s.cipher.Encrypt([]byte(verifier), oauthAssociatedData(hash, provider))
	if err != nil {
		return Start{}, err
	}
	if err := s.store.PutOAuthAttempt(ctx, storage.OAuthAttempt{
		StateHash: hash, Provider: provider, ConnectionID: connectionID, Mode: mode, EncryptedVerifier: encrypted, ReturnPath: returnPath, ExpiresAt: s.now().Add(attemptLifetime),
	}); err != nil {
		return Start{}, err
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	return Start{
		AuthorizationURL: client.AuthorizationURL(state, challenge),
		State:            state,
	}, nil
}

func (s *Service) Complete(ctx context.Context, provider, state, code string) (*oauth2.Token, string, error) {
	completion, err := s.CompleteWithTarget(ctx, provider, state, code)
	if err != nil {
		return nil, "", err
	}
	return completion.Token, completion.ReturnPath, nil
}

func (s *Service) CompleteWithTarget(ctx context.Context, provider, state, code string) (Completion, error) {
	client, ok := s.providers[provider]
	if !ok {
		return Completion{}, fmt.Errorf("unsupported OAuth provider %q", provider)
	}
	if state == "" || code == "" {
		return Completion{}, errors.New("OAuth state and code are required")
	}
	hash := stateHash(state)
	attempt, err := s.store.ConsumeOAuthAttempt(ctx, hash, s.now())
	if err != nil {
		return Completion{}, err
	}
	if attempt.Provider != provider {
		return Completion{}, errors.New("OAuth provider mismatch")
	}
	verifier, err := s.cipher.Decrypt(attempt.EncryptedVerifier, oauthAssociatedData(hash, provider))
	if err != nil {
		return Completion{}, err
	}
	token, err := client.Exchange(ctx, code, string(verifier))
	if err != nil {
		return Completion{}, fmt.Errorf("exchange OAuth code: %w", err)
	}
	return Completion{Token: token, ReturnPath: attempt.ReturnPath, ConnectionID: attempt.ConnectionID, Mode: attempt.Mode}, nil
}

func validReturnPath(path string) bool {
	return len(path) > 0 && path[0] == '/' && (len(path) == 1 || (path[1] != '/' && path[1] != '\\')) && !strings.ContainsAny(path, "\r\n")
}

func stateHash(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

func oauthAssociatedData(hash, provider string) []byte {
	return []byte("calendar-oauth\x00" + hash + "\x00" + provider)
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate OAuth secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
