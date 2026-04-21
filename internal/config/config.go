package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr     string
	RESTListenAddr string
	APIKey         string
	TokenDir       string

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
		ListenAddr:     envStr("LISTEN_ADDR", ":8080"),
		RESTListenAddr: envStr("REST_LISTEN_ADDR", ""),
		APIKey:         envStr("API_KEY", ""),
		TokenDir:       envStr("TOKEN_DIR", "/app/data"),

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
