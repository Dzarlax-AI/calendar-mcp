package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr           string
	RESTListenAddr       string
	WorkerHealthAddr     string
	APIKey               string
	AllowUnauthenticated bool
	EnableV2             bool
	TokenDir             string
	DatabaseURL          string
	EncryptionKey        string
	PublicURL            string
	TrustForwardAuth     bool

	GoogleClientID     string
	GoogleClientSecret string
	GoogleRefreshToken string

	MS365ClientID     string
	MS365ClientSecret string
	MS365TenantID     string
	MS365RefreshToken string

	AppleUsername    string
	AppleAppPassword string
	AppleCalDAVURL   string

	// Fan-out filtering for get_events without an explicit calendar_id.
	// Both settings only affect fan-out — list_calendars and explicit
	// calendar_id queries always see every calendar.
	ExcludeCalendarIDs       []string // prefixed IDs (e.g. "google:abc@import.calendar.google.com")
	IncludeImportedCalendars bool     // if true, disables the default @import.calendar.google.com skip
}

func Load() *Config {
	return &Config{
		ListenAddr:           envStr("LISTEN_ADDR", ":8080"),
		RESTListenAddr:       envStr("REST_LISTEN_ADDR", ""),
		WorkerHealthAddr:     envStr("WORKER_HEALTH_ADDR", "127.0.0.1:8082"),
		APIKey:               envStr("API_KEY", ""),
		AllowUnauthenticated: envBool("ALLOW_UNAUTHENTICATED", false),
		EnableV2:             envBool("ENABLE_V2", false),
		TokenDir:             envStr("TOKEN_DIR", "/app/data"),
		DatabaseURL:          envStr("DATABASE_URL", ""),
		EncryptionKey:        envStr("CALENDAR_ENCRYPTION_KEY", ""),
		PublicURL:            envStr("CALENDAR_PUBLIC_URL", ""),
		TrustForwardAuth:     envBool("UI_TRUST_FORWARD_AUTH", true),

		GoogleClientID:     envStr("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: envStr("GOOGLE_CLIENT_SECRET", ""),
		GoogleRefreshToken: envStr("GOOGLE_REFRESH_TOKEN", ""),

		MS365ClientID:     envStr("MS365_CLIENT_ID", ""),
		MS365ClientSecret: envStr("MS365_CLIENT_SECRET", ""),
		MS365TenantID:     envStr("MS365_TENANT_ID", ""),
		MS365RefreshToken: envStr("MS365_REFRESH_TOKEN", ""),

		AppleUsername:    envStr("APPLE_USERNAME", ""),
		AppleAppPassword: envStr("APPLE_APP_PASSWORD", ""),
		AppleCalDAVURL:   envStr("APPLE_CALDAV_URL", "https://caldav.icloud.com"),

		ExcludeCalendarIDs:       envList("EXCLUDE_CALENDAR_IDS"),
		IncludeImportedCalendars: envBool("INCLUDE_IMPORTED_CALENDARS", false),
	}
}

func (c *Config) Validate() error {
	if c.APIKey == "" && !c.AllowUnauthenticated {
		return fmt.Errorf("API_KEY is required unless ALLOW_UNAUTHENTICATED=true")
	}
	if c.DatabaseURL != "" && c.PublicURL == "" {
		return fmt.Errorf("CALENDAR_PUBLIC_URL is required when DATABASE_URL is configured")
	}
	if err := requireCompleteProvider("Google", map[string]string{"GOOGLE_CLIENT_ID": c.GoogleClientID, "GOOGLE_CLIENT_SECRET": c.GoogleClientSecret}); err != nil {
		return err
	}
	if err := requireCompleteProvider("Microsoft", map[string]string{"MS365_CLIENT_ID": c.MS365ClientID, "MS365_CLIENT_SECRET": c.MS365ClientSecret, "MS365_TENANT_ID": c.MS365TenantID}); err != nil {
		return err
	}
	if err := requireCompleteProvider("Apple", map[string]string{"APPLE_USERNAME": c.AppleUsername, "APPLE_APP_PASSWORD": c.AppleAppPassword}); err != nil {
		return err
	}
	return nil
}

func requireCompleteProvider(name string, values map[string]string) error {
	configured := false
	for _, value := range values {
		configured = configured || value != ""
	}
	if !configured {
		return nil
	}
	for key, value := range values {
		if value == "" {
			return fmt.Errorf("%s provider is partially configured: %s is required", name, key)
		}
	}
	return nil
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(os.Getenv(key))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envList(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
