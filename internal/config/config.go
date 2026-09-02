package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultEventCacheLookbackDays  = 365
	defaultEventCacheLookaheadDays = 730
	maxEventCacheDays              = 10 * 366
)

type Config struct {
	ListenAddr             string
	RESTListenAddr         string
	WorkerHealthAddr       string
	APIKey                 string
	LegacyAPIKey           string
	AllowUnauthenticated   bool
	EnableV2               bool
	TokenDir               string
	DatabaseURL            string
	EncryptionKey          string
	PublicURL              string
	TrustForwardAuth       bool
	UIAllowUnauthenticated bool
	UIRawArtifactUsers     []string
	// NativeAppToken is a dedicated bearer token for the native app API. It is
	// deliberately distinct from MCP and internal REST API keys; writes require
	// the separate NativeAppWritesEnabled opt-in.
	NativeAppToken string
	// ReadOnlyToken is a separate bearer token for projection-backed clients.
	// It only authorizes GET /api/native/v1/cached-events.
	ReadOnlyToken          string
	NativeAppTrustedProxy  bool
	NativeAppWritesEnabled bool

	// Event read model is deliberately opt-in. It only reads provider event
	// data; event mutations continue to use the existing notification-safe
	// application path.
	EventReadModelEnabled      bool
	EventCacheLookbackDays     int
	EventCacheLookaheadDays    int
	EventSyncGoogleInterval    time.Duration
	EventSyncMicrosoftInterval time.Duration
	EventSyncAppleInterval     time.Duration
	eventReadModelConfigErr    error

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
	cfg := &Config{
		ListenAddr:             envStr("LISTEN_ADDR", ":8080"),
		RESTListenAddr:         envStr("REST_LISTEN_ADDR", ""),
		WorkerHealthAddr:       envStr("WORKER_HEALTH_ADDR", "127.0.0.1:8082"),
		APIKey:                 envStr("API_KEY", ""),
		LegacyAPIKey:           envStr("API_KEY_LEGACY", ""),
		AllowUnauthenticated:   envBool("ALLOW_UNAUTHENTICATED", false),
		EnableV2:               envBool("ENABLE_V2", false),
		TokenDir:               envStr("TOKEN_DIR", "/app/data"),
		DatabaseURL:            envStr("DATABASE_URL", ""),
		EncryptionKey:          envStr("CALENDAR_ENCRYPTION_KEY", ""),
		PublicURL:              envStr("CALENDAR_PUBLIC_URL", ""),
		TrustForwardAuth:       envBool("UI_TRUST_FORWARD_AUTH", false),
		UIAllowUnauthenticated: envBool("UI_ALLOW_UNAUTHENTICATED", false),
		UIRawArtifactUsers:     envList("UI_RAW_ARTIFACT_USERS"),
		NativeAppToken:         envStr("NATIVE_APP_TOKEN", ""),
		ReadOnlyToken:          envStr("READ_ONLY_TOKEN", ""),
		NativeAppTrustedProxy:  envBool("NATIVE_APP_TRUSTED_PROXY", false),
		NativeAppWritesEnabled: envBool("NATIVE_APP_WRITES_ENABLED", false),

		EventReadModelEnabled:      envBool("EVENT_READ_MODEL_ENABLED", false),
		EventCacheLookbackDays:     envInt("EVENT_CACHE_LOOKBACK_DAYS", defaultEventCacheLookbackDays),
		EventCacheLookaheadDays:    envInt("EVENT_CACHE_LOOKAHEAD_DAYS", defaultEventCacheLookaheadDays),
		EventSyncGoogleInterval:    envDuration("EVENT_SYNC_GOOGLE_INTERVAL", time.Minute),
		EventSyncMicrosoftInterval: envDuration("EVENT_SYNC_MICROSOFT_INTERVAL", time.Minute),
		EventSyncAppleInterval:     envDuration("EVENT_SYNC_APPLE_INTERVAL", 5*time.Minute),

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
	if err := validateEventReadModelEnv(); err != nil {
		cfg.eventReadModelConfigErr = err
	}
	return cfg
}

func (c *Config) Validate() error {
	if c.APIKey == "" && c.NativeAppToken == "" && c.ReadOnlyToken == "" && !c.AllowUnauthenticated {
		return fmt.Errorf("API_KEY, NATIVE_APP_TOKEN, or READ_ONLY_TOKEN is required unless ALLOW_UNAUTHENTICATED=true")
	}
	if c.ReadOnlyToken != "" {
		for _, credential := range []struct {
			name, value string
		}{
			{"API_KEY", c.APIKey},
			{"API_KEY_LEGACY", c.LegacyAPIKey},
			{"NATIVE_APP_TOKEN", c.NativeAppToken},
		} {
			if c.ReadOnlyToken == credential.value && credential.value != "" {
				return fmt.Errorf("READ_ONLY_TOKEN must differ from %s", credential.name)
			}
		}
	}
	if (c.NativeAppToken != "" || c.ReadOnlyToken != "") && c.DatabaseURL == "" {
		return fmt.Errorf("native API tokens require DATABASE_URL")
	}
	if c.NativeAppToken != "" || c.ReadOnlyToken != "" {
		if err := validateNativeAppTransport(c.PublicURL, c.NativeAppTrustedProxy); err != nil {
			return err
		}
	}
	if c.NativeAppWritesEnabled && c.NativeAppToken == "" {
		return fmt.Errorf("NATIVE_APP_WRITES_ENABLED requires NATIVE_APP_TOKEN")
	}
	if c.DatabaseURL != "" && c.PublicURL == "" {
		return fmt.Errorf("CALENDAR_PUBLIC_URL is required when DATABASE_URL is configured")
	}
	if c.DatabaseURL != "" && c.NativeAppToken == "" && !c.TrustForwardAuth && !c.UIAllowUnauthenticated {
		return fmt.Errorf("platform UI requires UI_TRUST_FORWARD_AUTH=true unless UI_ALLOW_UNAUTHENTICATED=true is explicitly set")
	}
	// A Config assembled directly by a caller predates the optional read-model
	// fields. Load always supplies these defaults; retain that compatibility for
	// focused API configuration tests and external callers.
	if c.eventReadModelConfigErr != nil || c.EventReadModelEnabled || c.EventCacheLookbackDays != 0 || c.EventCacheLookaheadDays != 0 || c.EventSyncGoogleInterval != 0 || c.EventSyncMicrosoftInterval != 0 || c.EventSyncAppleInterval != 0 {
		return c.ValidateEventReadModel()
	}
	return nil
}

func validateNativeAppTransport(publicURL string, trustedProxy bool) error {
	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("native API tokens require a valid CALENDAR_PUBLIC_URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("native API tokens require CALENDAR_PUBLIC_URL to use HTTP or HTTPS")
	}
	isLoopback := parsed.Hostname() == "localhost"
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && ip.IsLoopback() {
		isLoopback = true
	}
	if isLoopback {
		return nil
	}
	if parsed.Scheme == "https" {
		if trustedProxy {
			return nil
		}
	}
	return fmt.Errorf("native API tokens require an HTTPS CALENDAR_PUBLIC_URL and NATIVE_APP_TRUSTED_PROXY=true unless it is loopback-only")
}

// ValidateEventReadModel checks bounds before the serve or worker process can
// create a rolling projection. Keeping this separate lets the worker validate
// its own inputs without requiring the HTTP API key.
func (c *Config) ValidateEventReadModel() error {
	if c.eventReadModelConfigErr != nil {
		return c.eventReadModelConfigErr
	}
	if c.EventCacheLookbackDays <= 0 || c.EventCacheLookbackDays > maxEventCacheDays {
		return fmt.Errorf("EVENT_CACHE_LOOKBACK_DAYS must be between 1 and %d", maxEventCacheDays)
	}
	if c.EventCacheLookaheadDays <= 0 || c.EventCacheLookaheadDays > maxEventCacheDays {
		return fmt.Errorf("EVENT_CACHE_LOOKAHEAD_DAYS must be between 1 and %d", maxEventCacheDays)
	}
	for _, item := range []struct {
		name     string
		interval time.Duration
	}{
		{"EVENT_SYNC_GOOGLE_INTERVAL", c.EventSyncGoogleInterval},
		{"EVENT_SYNC_MICROSOFT_INTERVAL", c.EventSyncMicrosoftInterval},
		{"EVENT_SYNC_APPLE_INTERVAL", c.EventSyncAppleInterval},
	} {
		if item.interval <= 0 || item.interval > 24*time.Hour {
			return fmt.Errorf("%s must be greater than zero and no more than 24h", item.name)
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

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func validateEventReadModelEnv() error {
	for _, key := range []string{"EVENT_SYNC_GOOGLE_INTERVAL", "EVENT_SYNC_MICROSOFT_INTERVAL", "EVENT_SYNC_APPLE_INTERVAL"} {
		if value := os.Getenv(key); value != "" {
			if _, err := time.ParseDuration(value); err != nil {
				return fmt.Errorf("%s must be a Go duration: %w", key, err)
			}
		}
	}
	for _, key := range []string{"EVENT_CACHE_LOOKBACK_DAYS", "EVENT_CACHE_LOOKAHEAD_DAYS"} {
		if value := os.Getenv(key); value != "" {
			if _, err := strconv.Atoi(value); err != nil {
				return fmt.Errorf("%s must be an integer: %w", key, err)
			}
		}
	}
	return nil
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
