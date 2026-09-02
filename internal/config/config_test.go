package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadEventReadModelDefaults(t *testing.T) {
	for _, key := range []string{
		"EVENT_READ_MODEL_ENABLED", "EVENT_CACHE_LOOKBACK_DAYS", "EVENT_CACHE_LOOKAHEAD_DAYS",
		"EVENT_SYNC_GOOGLE_INTERVAL", "EVENT_SYNC_MICROSOFT_INTERVAL", "EVENT_SYNC_APPLE_INTERVAL",
	} {
		t.Setenv(key, "")
	}
	cfg := Load()
	if cfg.EventReadModelEnabled {
		t.Fatal("event read model is enabled by default")
	}
	if cfg.EventCacheLookbackDays != 365 || cfg.EventCacheLookaheadDays != 730 {
		t.Fatalf("cache window = %d/%d, want 365/730", cfg.EventCacheLookbackDays, cfg.EventCacheLookaheadDays)
	}
	if cfg.EventSyncGoogleInterval != time.Minute || cfg.EventSyncMicrosoftInterval != time.Minute || cfg.EventSyncAppleInterval != 5*time.Minute {
		t.Fatalf("intervals = %s/%s/%s", cfg.EventSyncGoogleInterval, cfg.EventSyncMicrosoftInterval, cfg.EventSyncAppleInterval)
	}
	if err := cfg.ValidateEventReadModel(); err != nil {
		t.Fatalf("ValidateEventReadModel() error = %v", err)
	}
}

func TestValidateEventReadModelRejectsUnsafeValues(t *testing.T) {
	cfg := Load()
	cfg.EventCacheLookbackDays = 0
	if err := cfg.ValidateEventReadModel(); err == nil {
		t.Fatal("accepted non-positive lookback")
	}
	cfg = Load()
	cfg.EventSyncGoogleInterval = 0
	if err := cfg.ValidateEventReadModel(); err == nil {
		t.Fatal("accepted non-positive Google interval")
	}
	cfg = Load()
	cfg.EventCacheLookaheadDays = maxEventCacheDays + 1
	if err := cfg.ValidateEventReadModel(); err == nil {
		t.Fatal("accepted unsafe cache lookahead")
	}
	t.Setenv("EVENT_SYNC_APPLE_INTERVAL", "not-a-duration")
	if err := Load().ValidateEventReadModel(); err == nil || !strings.Contains(err.Error(), "EVENT_SYNC_APPLE_INTERVAL") {
		t.Fatalf("invalid duration error = %v", err)
	}
}

func TestLoadReadsLegacyAPIKey(t *testing.T) {
	t.Setenv("API_KEY", "primary")
	t.Setenv("API_KEY_LEGACY", "legacy")

	cfg := Load()
	if cfg.APIKey != "primary" || cfg.LegacyAPIKey != "legacy" {
		t.Fatalf("keys = %q/%q", cfg.APIKey, cfg.LegacyAPIKey)
	}
}

func TestLoadRequiresExplicitForwardAuthOptIn(t *testing.T) {
	t.Setenv("UI_TRUST_FORWARD_AUTH", "")
	if cfg := Load(); cfg.TrustForwardAuth {
		t.Fatal("TrustForwardAuth = true without explicit opt-in")
	}

	t.Setenv("UI_TRUST_FORWARD_AUTH", "true")
	if cfg := Load(); !cfg.TrustForwardAuth {
		t.Fatal("TrustForwardAuth = false with explicit opt-in")
	}
}

func TestValidateRejectsMissingAPIKeyByDefault(t *testing.T) {
	cfg := &Config{}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil, want missing API key error")
	}
}

func TestValidateRejectsLegacyKeyWithoutPrimaryKey(t *testing.T) {
	cfg := &Config{LegacyAPIKey: "legacy"}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil, want missing primary API key error")
	}
}

func TestValidateAllowsExplicitUnauthenticatedMode(t *testing.T) {
	cfg := &Config{AllowUnauthenticated: true}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateAllowsAPIKey(t *testing.T) {
	cfg := &Config{APIKey: "secret"}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateAllowsDedicatedNativeAppToken(t *testing.T) {
	cfg := &Config{
		NativeAppToken: "native-token",
		DatabaseURL:    "sqlite:///tmp/calendar.db",
		PublicURL:      "http://localhost:8080",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateAllowsReadOnlyToken(t *testing.T) {
	cfg := &Config{
		ReadOnlyToken:          "read-only-token",
		DatabaseURL:            "sqlite:///tmp/calendar.db",
		PublicURL:              "http://localhost:8080",
		UIAllowUnauthenticated: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsReadOnlyTokenCollisions(t *testing.T) {
	base := Config{
		APIKey:                 "api-token",
		LegacyAPIKey:           "legacy-token",
		NativeAppToken:         "native-token",
		ReadOnlyToken:          "read-only-token",
		DatabaseURL:            "sqlite:///tmp/calendar.db",
		PublicURL:              "http://localhost:8080",
		UIAllowUnauthenticated: true,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("distinct credentials must validate: %v", err)
	}
	for _, token := range []string{base.APIKey, base.LegacyAPIKey, base.NativeAppToken} {
		cfg := base
		cfg.ReadOnlyToken = token
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate() accepted READ_ONLY_TOKEN collision with %q", token)
		}
	}
}

func TestValidateRejectsNativeWritesWithoutNativeToken(t *testing.T) {
	cfg := &Config{NativeAppWritesEnabled: true, DatabaseURL: "sqlite:///tmp/calendar.db"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted native writes without NATIVE_APP_TOKEN")
	}
}

func TestValidateRejectsUnsafeNativeAppTransport(t *testing.T) {
	cfg := &Config{
		NativeAppToken: "native-token",
		DatabaseURL:    "sqlite:///tmp/calendar.db",
		PublicURL:      "https://calendar.example.com",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted an external native API without an explicit trusted proxy")
	}
	cfg.NativeAppTrustedProxy = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected HTTPS native API behind a trusted proxy: %v", err)
	}
	cfg.PublicURL = "http://calendar.example.com"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted an external HTTP native API URL")
	}
	cfg.PublicURL = "ftp://localhost"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted a non-HTTP loopback native API URL")
	}
}

func TestValidateRejectsNativeAppTokenWithoutPlatformStorage(t *testing.T) {
	cfg := &Config{NativeAppToken: "native-token"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted NATIVE_APP_TOKEN without DATABASE_URL")
	}
}

func TestValidateReadOnlyTokenUsesTokenNeutralTransportErrors(t *testing.T) {
	cfg := &Config{
		ReadOnlyToken:          "read-only-token",
		DatabaseURL:            "sqlite:///tmp/calendar.db",
		PublicURL:              "http://calendar.example.com",
		NativeAppTrustedProxy:  false,
		UIAllowUnauthenticated: true,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an unsafe READ_ONLY_TOKEN transport")
	}
	if !strings.Contains(err.Error(), "native API tokens") {
		t.Fatalf("Validate() error = %q, want token-neutral wording", err)
	}
}

func TestValidateAllowsPartialProviderConfigurationToBeSkipped(t *testing.T) {
	cfg := &Config{APIKey: "secret", GoogleClientID: "client"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected skippable partial provider: %v", err)
	}
}

func TestValidateRequiresExplicitUIAuthenticationMode(t *testing.T) {
	cfg := &Config{APIKey: "secret", DatabaseURL: "sqlite:///tmp/calendar.db", PublicURL: "http://localhost:8080"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted platform UI without ForwardAuth or explicit local bypass")
	}
	cfg.UIAllowUnauthenticated = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected explicit unauthenticated UI mode: %v", err)
	}
	cfg = &Config{NativeAppToken: "native-token", DatabaseURL: "sqlite:///tmp/calendar.db", PublicURL: "http://localhost:8080"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected dedicated native-token mode: %v", err)
	}
}
