package runtime

import (
	"testing"
	"time"

	"calendar-mcp/internal/config"
)

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
