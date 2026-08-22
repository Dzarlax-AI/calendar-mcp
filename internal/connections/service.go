package connections

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	return s.create(ctx, id, provider, displayName, oauthToken, scopes)
}

func (s *Service) ConnectOAuth(ctx context.Context, provider, displayName string, oauthToken *oauth2.Token, scopes []string) (string, error) {
	id := uuid.NewString()
	if err := s.CreateOAuth(ctx, id, provider, displayName, oauthToken, scopes); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) ReconnectOAuth(ctx context.Context, id, provider string, oauthToken *oauth2.Token) error {
	if oauthToken == nil {
		return errors.New("OAuth token is required")
	}
	record, err := s.store.ConnectionByID(ctx, id)
	if err != nil {
		return err
	}
	if record.Provider != provider {
		return errors.New("connection provider mismatch")
	}
	tokenStore := s.OAuthTokenStore(id, provider)
	replacement := *oauthToken
	if replacement.RefreshToken == "" {
		previous, loadErr := tokenStore.Load()
		if loadErr != nil {
			return loadErr
		}
		replacement.RefreshToken = previous.RefreshToken
	}
	if err := tokenStore.Save(&replacement); err != nil {
		return err
	}
	return s.store.UpdateConnectionVerification(ctx, id, "connected", "", s.now())
}

func (s *Service) CreateApple(ctx context.Context, id, displayName, username, appPassword string) error {
	if username == "" || appPassword == "" {
		return errors.New("apple username and app-specific password are required")
	}
	return s.create(ctx, id, "apple", displayName, AppleCredentials{Username: username, AppPassword: appPassword}, nil)
}

func (s *Service) ConnectApple(ctx context.Context, displayName, username, appPassword string) (string, error) {
	id := uuid.NewString()
	if err := s.CreateApple(ctx, id, displayName, username, appPassword); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) ReconnectApple(ctx context.Context, id, username, appPassword string) error {
	if strings.TrimSpace(username) == "" || appPassword == "" {
		return errors.New("apple username and app-specific password are required")
	}
	record, err := s.store.ConnectionByID(ctx, id)
	if err != nil {
		return err
	}
	if record.Provider != "apple" {
		return errors.New("connection provider mismatch")
	}
	plaintext, err := json.Marshal(AppleCredentials{Username: username, AppPassword: appPassword})
	if err != nil {
		return err
	}
	encrypted, err := s.cipher.Encrypt(plaintext, credentials.AssociatedData(id, "apple"))
	if err != nil {
		return err
	}
	return s.store.UpdateConnectionCredentials(ctx, id, encrypted, credentialVersion, s.now())
}

func (s *Service) create(ctx context.Context, id, provider, displayName string, value any, scopes []string) error {
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
		ID: id, Provider: provider, DisplayName: displayName, Status: "connected",
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

func (s *Service) VerifyAndDiscover(ctx context.Context, connectionID string, provider calendar.Provider) error {
	record, err := s.store.ConnectionByID(ctx, connectionID)
	if err != nil {
		return err
	}
	if record.Provider != provider.Name() {
		return errors.New("connection provider mismatch")
	}
	calendars, err := provider.ListCalendars(ctx)
	if err != nil {
		_ = s.store.UpdateConnectionVerification(ctx, connectionID, "error", "provider_verification_failed", s.now())
		return fmt.Errorf("verify %s connection: %w", provider.Name(), err)
	}
	now := s.now()
	fingerprintSource, displayName := connectionIdentity(record, calendars)
	if record.Provider == "apple" {
		if appleCredentials, loadErr := s.LoadApple(ctx, connectionID); loadErr == nil {
			fingerprintSource = strings.ToLower(strings.TrimSpace(appleCredentials.Username))
			displayName = appleCredentials.Username
		}
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(record.Provider+"\x00"+fingerprintSource)))
	if err := s.store.UpdateConnectionIdentity(ctx, connectionID, fingerprint, displayName, now); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			_ = s.store.UpdateConnectionVerification(ctx, connectionID, "error", "duplicate_provider_account", now)
			return errors.New("this provider account is already connected")
		}
		return err
	}
	for _, discovered := range calendars {
		canWrite := !discovered.ReadOnly
		supportsRecurrence := false
		if capable, ok := provider.(calendar.CapabilityProvider); ok {
			capabilities, capabilityErr := capable.Capabilities(ctx, discovered.ID)
			if capabilityErr != nil {
				_ = s.store.UpdateConnectionVerification(ctx, connectionID, "error", "provider_discovery_failed", s.now())
				return fmt.Errorf("discover calendar capabilities: %w", capabilityErr)
			}
			canWrite = capabilities.Operations.Create || capabilities.Operations.Update
			supportsRecurrence = capabilities.Fields.Recurrence
		}
		if err := s.store.UpsertCalendar(ctx, storage.Calendar{
			ID: calendar.AccountCalendarID(provider.Name(), connectionID, discovered.ID), ConnectionID: connectionID, ProviderCalendarID: discovered.ID,
			Name: discovered.Name, CanRead: true, CanWrite: canWrite, SupportsRecurrence: supportsRecurrence, DiscoveredAt: now,
		}); err != nil {
			_ = s.store.UpdateConnectionVerification(ctx, connectionID, "error", "provider_discovery_failed", s.now())
			return err
		}
	}
	return s.store.UpdateConnectionVerification(ctx, connectionID, "connected", "", now)
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
	service      *Service
	connectionID string
	provider     string
}

func (s *databaseTokenStore) Load() (*oauth2.Token, error) {
	var value oauth2.Token
	if err := s.service.decrypt(context.Background(), s.connectionID, s.provider, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *databaseTokenStore) Save(value *oauth2.Token) error {
	plaintext, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encrypted, err := s.service.cipher.Encrypt(plaintext, credentials.AssociatedData(s.connectionID, s.provider))
	if err != nil {
		return err
	}
	return s.service.store.PersistConnectionCredentials(context.Background(), s.connectionID, encrypted, credentialVersion, s.service.now())
}
