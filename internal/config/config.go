package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr string
	APIKey     string
	TokenDir   string

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
}

func Load() *Config {
	return &Config{
		ListenAddr: envStr("LISTEN_ADDR", ":8080"),
		APIKey:     envStr("API_KEY", ""),
		TokenDir:   envStr("TOKEN_DIR", "/app/data"),

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
	}
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
