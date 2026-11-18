package connections

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/token"
)

const credentialVersion = 1

type Service struct {
	store  *storage.Store
	cipher *credentials.Cipher
	now    func() time.Time
}

type AppleCredentials struct {
	Username    string `json:"username"`
	AppPassword string `json:"app_password"`
}

func New(store *storage.Store, cipher *credentials.Cipher) *Service {
	return &Service{store: store, cipher: cipher, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) CreateOAuth(ctx context.Context, id, provider, displayName string, oauthToken *oauth2.Token, scopes []string) error {
	if provider != "google" && provider != "microsoft" {
		return fmt.Errorf("unsupported OAuth provider %q", provider)
	}
	return s.create(ctx, id, provider, displayName, oauthToken, scopes, "connected")
}

func (s *Service) ConnectOAuth(ctx context.Context, provider, displayName string, oauthToken *oauth2.Token, scopes []string) (string, error) {
	id := uuid.NewString()
	if provider != "google" && provider != "microsoft" {
		return "", fmt.Errorf("unsupported OAuth provider %q", provider)
	}
	if err := s.create(ctx, id, provider, displayName, oauthToken, scopes, "pending"); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) ReconnectOAuth(ctx context.Context, id, provider string, oauthToken *oauth2.Token) error {
	_, err := s.ReconnectOAuthSnapshot(ctx, id, provider, oauthToken)
	return err
}

// ReconnectOAuthSnapshot returns the exact authorization revision created by this attempt.
func (s *Service) ReconnectOAuthSnapshot(ctx context.Context, id, provider string, oauthToken *oauth2.Token) (storage.CredentialSnapshot, error) {
	if oauthToken == nil {
		return storage.CredentialSnapshot{}, errors.New("OAuth token is required")
	}
	record, err := s.store.ConnectionByID(ctx, id)
	if err != nil {
		return storage.CredentialSnapshot{}, err
	}
	if record.Provider != provider {
		return storage.CredentialSnapshot{}, errors.New("connection provider mismatch")
	}
	if provider == "google" && oauthToken.RefreshToken == "" {
		return storage.CredentialSnapshot{}, errors.New("Google reconnect requires its own refresh token")
	}
	replacement := *oauthToken
	if replacement.RefreshToken == "" {
		previous, loadErr := s.OAuthTokenStore(id, provider).Load()
		if loadErr != nil {
			return storage.CredentialSnapshot{}, loadErr
		}
		replacement.RefreshToken = previous.RefreshToken
	}

	plaintext, err := json.Marshal(&replacement)
	if err != nil {
		return storage.CredentialSnapshot{}, err
	}
	encrypted, err := s.cipher.Encrypt(plaintext, credentials.AssociatedData(id, provider))
	if err != nil {
		return storage.CredentialSnapshot{}, err
	}
	return s.store.BeginAuthorization(ctx, id, encrypted, s.now())
}

func (s *Service) CreateApple(ctx context.Context, id, displayName, username, appPassword string) error {
	if username == "" || appPassword == "" {
		return errors.New("apple username and app-specific password are required")
	}
	return s.create(ctx, id, "apple", displayName, AppleCredentials{Username: username, AppPassword: appPassword}, nil, "connected")
}

func (s *Service) ConnectApple(ctx context.Context, displayName, username, appPassword string) (string, error) {
	id := uuid.NewString()
	if username == "" || appPassword == "" {
		return "", errors.New("apple username and app-specific password are required")
	}
	if err := s.create(ctx, id, "apple", displayName, AppleCredentials{Username: username, AppPassword: appPassword}, nil, "pending"); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) ReconnectApple(ctx context.Context, id, username, appPassword string) error {
	_, err := s.ReconnectAppleSnapshot(ctx, id, username, appPassword)
	return err
}

// ReconnectAppleSnapshot returns the exact authorization revision created by this attempt.
func (s *Service) ReconnectAppleSnapshot(ctx context.Context, id, username, appPassword string) (storage.CredentialSnapshot, error) {
	if strings.TrimSpace(username) == "" || appPassword == "" {
		return storage.CredentialSnapshot{}, errors.New("apple username and app-specific password are required")
	}
	record, err := s.store.ConnectionByID(ctx, id)
	if err != nil {
		return storage.CredentialSnapshot{}, err
	}
	if record.Provider != "apple" {
		return storage.CredentialSnapshot{}, errors.New("connection provider mismatch")
	}
	plaintext, err := json.Marshal(AppleCredentials{Username: username, AppPassword: appPassword})
	if err != nil {
		return storage.CredentialSnapshot{}, err
	}
	encrypted, err := s.cipher.Encrypt(plaintext, credentials.AssociatedData(id, "apple"))
	if err != nil {
		return storage.CredentialSnapshot{}, err
	}
	return s.store.BeginAuthorization(ctx, id, encrypted, s.now())
}

func (s *Service) create(ctx context.Context, id, provider, displayName string, value any, scopes []string, status string) error {
	if id == "" || displayName == "" {
		return errors.New("connection id and display name are required")
	}
	plaintext, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	encrypted, err := s.cipher.Encrypt(plaintext, credentials.AssociatedData(id, provider))
	if err != nil {
		return err
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return err
	}
	now := s.now()
	return s.store.CreateConnection(ctx, storage.Connection{
		ID: id, Provider: provider, DisplayName: displayName, Status: status,
		EncryptedCredentials: encrypted, CredentialVersion: credentialVersion, ScopesJSON: string(scopesJSON),
		CreatedAt: now, UpdatedAt: now,
	})
}

func (s *Service) OAuthTokenStore(connectionID, provider string) token.Store {
	return &databaseTokenStore{service: s, connectionID: connectionID, provider: provider}
}

func (s *Service) LoadApple(ctx context.Context, connectionID string) (AppleCredentials, error) {
	var value AppleCredentials
	if err := s.decrypt(ctx, connectionID, "apple", &value); err != nil {
		return value, err
	}
	return value, nil
}

// LoadAppleSnapshot decrypts the captured record without re-reading storage.
func (s *Service) LoadAppleSnapshot(snapshot storage.CredentialSnapshot) (AppleCredentials, error) {
	var value AppleCredentials
	if snapshot.Provider != "apple" || snapshot.CredentialVersion != credentialVersion {
		return value, errors.New("credential snapshot mismatch")
	}
	plaintext, err := s.cipher.Decrypt(snapshot.EncryptedCredentials, credentials.AssociatedData(snapshot.ConnectionID, "apple"))
	if err != nil {
		return value, err
	}
	if err = json.Unmarshal(plaintext, &value); err != nil {
		return value, errors.New("decode stored credentials")
	}
	return value, nil
}

// VerifyNonGoogle constructs the verifier from the captured credential snapshot.
func (s *Service) VerifyNonGoogle(ctx context.Context, id string, build func(storage.CredentialSnapshot) (calendar.Provider, error)) (resultErr error) {
	snapshot, err := s.store.CredentialSnapshot(ctx, id)
	if err != nil {
		return err
	}
	return s.VerifyNonGoogleSnapshot(ctx, snapshot, build)
}

// VerifyNonGoogleSnapshot verifies and cleans up only the captured authorization revision.
func (s *Service) VerifyNonGoogleSnapshot(ctx context.Context, snapshot storage.CredentialSnapshot, build func(storage.CredentialSnapshot) (calendar.Provider, error)) (resultErr error) {
	defer func() {
		if resultErr != nil && snapshot.Status == "pending" {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = s.store.FailAuthorization(cleanup, snapshot, s.now())
		}
	}()
	if build == nil {
		return errors.New("snapshot-bound verification unavailable")
	}
	provider, err := build(snapshot)
	if err != nil {
		return err
	}
	return s.verifyNonGoogleSnapshot(ctx, snapshot, provider)
}

func (s *Service) verifyNonGoogleSnapshot(ctx context.Context, snapshot storage.CredentialSnapshot, provider calendar.Provider) (resultErr error) {
	if provider == nil || snapshot.Provider == "google" || snapshot.Provider != provider.Name() {
		return errors.New("connection provider mismatch")
	}

	calendars, err := provider.ListCalendars(ctx)
	if err != nil {
		return fmt.Errorf("verify %s connection: %w", provider.Name(), err)
	}
	now := s.now()
	record, err := s.store.ConnectionByID(ctx, snapshot.ConnectionID)
	if err != nil {
		return err
	}
	identity, display := connectionIdentity(record, calendars)
	if snapshot.Provider == "apple" {
		creds, e := s.LoadAppleSnapshot(snapshot)
		if e != nil {
			return e
		}
		identity, display = strings.ToLower(strings.TrimSpace(creds.Username)), creds.Username
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(snapshot.Provider+"\x00"+identity)))
	staged := make([]storage.Calendar, 0, len(calendars))
	for _, c := range calendars {
		canWrite, recurrence := !c.ReadOnly, false
		if capable, ok := provider.(calendar.CapabilityProvider); ok {
			caps, e := capable.Capabilities(ctx, c.ID)
			if e != nil {
				return fmt.Errorf("discover calendar capabilities: %w", e)
			}
			canWrite, recurrence = caps.Operations.Create || caps.Operations.Update, caps.Fields.Recurrence
		}
		staged = append(staged, storage.Calendar{ID: calendar.AccountCalendarID(snapshot.Provider, snapshot.ConnectionID, c.ID), ConnectionID: snapshot.ConnectionID, ProviderCalendarID: c.ID, Name: c.Name, Timezone: c.Timezone, CanRead: true, CanWrite: canWrite, SupportsRecurrence: recurrence, DiscoveredAt: now})
	}
	return s.store.CompleteNonGoogleAuthorization(ctx, snapshot, staged, fingerprint, display, now)
}

func connectionIdentity(record storage.Connection, calendars []calendar.Calendar) (string, string) {
	if len(calendars) == 0 {
		return record.ID, record.DisplayName
	}
	ordered := append([]calendar.Calendar(nil), calendars...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Primary != ordered[j].Primary {
			return ordered[i].Primary
		}
		return ordered[i].ID < ordered[j].ID
	})
	name := ordered[0].Name
	if name == "" {
		name = record.DisplayName
	}
	return ordered[0].ID, name
}

func (s *Service) Delete(ctx context.Context, connectionID string) error {
	return s.store.DeleteConnection(ctx, connectionID)
}

func (s *Service) decrypt(ctx context.Context, id, provider string, out any) error {
	connection, err := s.store.ConnectionByID(ctx, id)
	if err != nil {
		return err
	}
	if connection.Provider != provider {
		return errors.New("connection provider mismatch")
	}
	if connection.CredentialVersion != credentialVersion {
		return errors.New("unsupported credential version")
	}
	plaintext, err := s.cipher.Decrypt(connection.EncryptedCredentials, credentials.AssociatedData(id, provider))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(plaintext, out); err != nil {
		return errors.New("decode stored credentials")
	}
	return nil
}

type databaseTokenStore struct {
	mu           sync.Mutex
	snapshot     *storage.CredentialSnapshot
	plaintext    []byte
	service      *Service
	connectionID string
	provider     string
}

// SnapshotOAuthTokenStore binds provider construction and every refresh to one
// credential snapshot; it never reloads a newer token behind an old provider.
func (s *Service) SnapshotOAuthTokenStore(snapshot storage.CredentialSnapshot) token.Store {
	return &databaseTokenStore{service: s, connectionID: snapshot.ConnectionID, provider: snapshot.Provider, snapshot: &snapshot}
}

func (s *databaseTokenStore) Load() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := s.service.store.CredentialSnapshot(ctx, s.connectionID)
		if err != nil {
			return nil, err
		}
		s.snapshot = &snap
	}
	if s.snapshot.Provider != s.provider || s.snapshot.CredentialVersion != credentialVersion {
		return nil, errors.New("credential snapshot mismatch")
	}
	plaintext, err := s.service.cipher.Decrypt(s.snapshot.EncryptedCredentials, credentials.AssociatedData(s.connectionID, s.provider))
	if err != nil {
		return nil, err
	}
	var value oauth2.Token
	if err = json.Unmarshal(plaintext, &value); err != nil {
		return nil, errors.New("decode stored credentials")
	}
	s.plaintext, err = json.Marshal(&value)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *databaseTokenStore) Save(value *oauth2.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		snap, err := s.service.store.CredentialSnapshot(ctx, s.connectionID)
		cancel()
		if err != nil {
			return err
		}
		s.snapshot = &snap
		old, err := s.service.cipher.Decrypt(snap.EncryptedCredentials, credentials.AssociatedData(s.connectionID, s.provider))
		if err != nil {
			return err
		}
		var previous oauth2.Token
		if err = json.Unmarshal(old, &previous); err != nil {
			return err
		}
		s.plaintext, err = json.Marshal(&previous)
		if err != nil {
			return err
		}
	}
	plaintext, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if bytes.Equal(plaintext, s.plaintext) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.service.store.ValidateCredentialSnapshot(ctx, *s.snapshot)
	}
	encrypted, err := s.service.cipher.Encrypt(plaintext, credentials.AssociatedData(s.connectionID, s.provider))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = s.service.store.CompareAndSwapCredentials(ctx, *s.snapshot, encrypted, s.service.now()); err != nil {
		return err
	}
	s.snapshot.EncryptedCredentials = encrypted
	s.plaintext = plaintext
	return nil
}

// VerifyGoogle starts authority invalidation before constructing the verifier.
// The builder must only use the provided snapshot.
func (s *Service) VerifyGoogle(ctx context.Context, id string, build func(storage.CredentialSnapshot) (calendar.Provider, error)) (resultErr error) {
	snapshot, err := s.store.BeginAuthorization(ctx, id, nil, s.now())
	if err != nil {
		return err
	}
	return s.VerifyGoogleSnapshot(ctx, snapshot, build)
}

// VerifyGoogleSnapshot completes an already-fenced verification attempt.
func (s *Service) VerifyGoogleSnapshot(ctx context.Context, snapshot storage.CredentialSnapshot, build func(storage.CredentialSnapshot) (calendar.Provider, error)) (resultErr error) {
	id := snapshot.ConnectionID
	defer func() {
		if resultErr != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = s.store.FailAuthorization(cleanup, snapshot, s.now())
		}
	}()
	if snapshot.Provider != "google" || build == nil {
		return errors.New("Google verification unavailable")
	}
	provider, err := build(snapshot)
	if err != nil {
		return err
	}
	discovery, ok := provider.(calendar.DetailDiscoveryProvider)
	if !ok {
		return errors.New("Google role discovery unavailable")
	}
	entries, err := discovery.DiscoverDetailCalendars(ctx)
	if err != nil {
		return err
	}
	staged := make([]storage.DiscoveredDetailCalendar, 0, len(entries))
	for _, entry := range entries {
		c := entry.Calendar
		staged = append(staged, storage.DiscoveredDetailCalendar{Calendar: storage.Calendar{ID: calendar.AccountCalendarID("google", id, c.ID), ConnectionID: id, ProviderCalendarID: c.ID, Name: c.Name, Timezone: c.Timezone, GooglePrimary: c.Primary}, AccessRole: entry.AccessRole})
	}
	record, err := s.store.ConnectionByID(ctx, id)
	if err != nil {
		return err
	}
	calendars := make([]calendar.Calendar, 0, len(entries))
	for _, entry := range entries {
		calendars = append(calendars, entry.Calendar)
	}
	identity, display := connectionIdentity(record, calendars)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte("google\x00"+identity)))
	return s.store.CompleteGoogleAuthorizationWithIdentity(ctx, snapshot, staged, fingerprint, display, s.now())
}

// CredentialBinding returns the current snapshot held by this token source.
func (s *databaseTokenStore) CredentialBinding() (string, [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		return "", [32]byte{}
	}
	return s.snapshot.AuthorizationRevision, sha256.Sum256(s.snapshot.EncryptedCredentials)
}
