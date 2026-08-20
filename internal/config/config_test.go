package config

import "testing"

func TestValidateRejectsMissingAPIKeyByDefault(t *testing.T) {
	cfg := &Config{}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil, want missing API key error")
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

func TestValidateRejectsPartialProviderConfiguration(t *testing.T) {
	cfg := &Config{APIKey: "secret", GoogleClientID: "client"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted a provider with missing client secret")
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
