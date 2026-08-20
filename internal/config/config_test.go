package config

import "testing"

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
}
