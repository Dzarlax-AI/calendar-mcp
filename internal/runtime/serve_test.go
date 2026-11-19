package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/config"
)

func TestServeRejectsRemovedMCPModesBeforeInitializingProviders(t *testing.T) {
	for _, mode := range []string{"compat", "modren"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MCP_CATALOG_MODE", mode)
			t.Setenv("ENABLE_V2", "false")
			err := Serve(context.Background())
			if err == nil || !strings.Contains(err.Error(), "configuration: MCP_CATALOG_MODE") {
				t.Fatalf("Serve() = %v, want catalog configuration error before initialization", err)
			}
		})
	}
}

func TestServeRejectsStandaloneMCP(t *testing.T) {
	t.Setenv("API_KEY", "fixture")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REST_LISTEN_ADDR", "")
	t.Setenv("MCP_CATALOG_MODE", "")
	t.Setenv("GATEWAY_CACHED_READS_PREPARE", "")
	t.Setenv("GATEWAY_CACHED_READS_ONLY", "")
	err := Serve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "calendar MCP requires platform storage") {
		t.Fatalf("Serve() = %v, want standalone MCP removal error", err)
	}
}

func TestWebRuntimeConfigUsesParsedEventReadModelFlag(t *testing.T) {
	for _, test := range []struct {
		name string
		env  string
		want bool
	}{
		{name: "enabled alias", env: "yes", want: true},
		{name: "disabled alias", env: "off", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EVENT_READ_MODEL_ENABLED", test.env)
			cfg := config.Load()
			anchor := time.Date(2026, 8, 22, 16, 45, 0, 0, time.FixedZone("CEST", 2*60*60))
			webCfg := webRuntimeConfigAt(cfg, nil, nil, anchor)
			if webCfg.EventReadModelEnabled == nil || *webCfg.EventReadModelEnabled != test.want {
				t.Fatalf("EventReadModelEnabled = %v, want %t", webCfg.EventReadModelEnabled, test.want)
			}
			if test.want {
				if !webCfg.EventReadModelWindow.End.After(webCfg.EventReadModelWindow.Start) {
					t.Fatalf("enabled web window is invalid: %#v", webCfg.EventReadModelWindow)
				}
				if want := eventReadModelWindow(cfg, anchor); webCfg.EventReadModelWindow != want {
					t.Fatalf("enabled web window = %#v, want %#v", webCfg.EventReadModelWindow, want)
				}
			} else if !webCfg.EventReadModelWindow.Start.IsZero() || !webCfg.EventReadModelWindow.End.IsZero() {
				t.Fatalf("disabled web window = %#v, want zero", webCfg.EventReadModelWindow)
			}
		})
	}
}
