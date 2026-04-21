package main

import (
	"log"
	"net/http"

	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/google"
	"calendar-mcp/internal/mcpserver"
	"calendar-mcp/internal/microsoft"
	"calendar-mcp/internal/restapi"
)

func main() {
	cfg := config.Load()

	var providers []calendar.Provider

	if cfg.GoogleClientID != "" {
		g, err := google.New(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRefreshToken, cfg.TokenDir)
		if err != nil {
			log.Fatalf("google provider: %v", err)
		}
		providers = append(providers, g)
		log.Println("google calendar provider enabled")
	}

	if cfg.MS365ClientID != "" {
		m, err := microsoft.New(cfg.MS365ClientID, cfg.MS365ClientSecret, cfg.MS365TenantID, cfg.MS365RefreshToken, cfg.TokenDir)
		if err != nil {
			log.Fatalf("microsoft provider: %v", err)
		}
		providers = append(providers, m)
		log.Println("microsoft calendar provider enabled")
	}

	if cfg.AppleUsername != "" {
		a, err := apple.New(cfg.AppleUsername, cfg.AppleAppPassword, cfg.AppleCalDAVURL)
		if err != nil {
			log.Fatalf("apple provider: %v", err)
		}
		providers = append(providers, a)
		log.Println("apple calendar provider enabled")
	}

	if len(providers) == 0 {
		log.Fatal("no calendar providers configured")
	}

	reg := calendar.NewRegistry(providers, calendar.RegistryOptions{
		ExcludeIDs:               cfg.ExcludeCalendarIDs,
		IncludeImportedCalendars: cfg.IncludeImportedCalendars,
	})
	if len(cfg.ExcludeCalendarIDs) > 0 {
		log.Printf("fan-out excludes %d calendar(s): %v", len(cfg.ExcludeCalendarIDs), cfg.ExcludeCalendarIDs)
	}
	if !cfg.IncludeImportedCalendars {
		log.Printf("fan-out auto-skips google:*@import.calendar.google.com (set INCLUDE_IMPORTED_CALENDARS=true to disable)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mcpserver.Register(mux, reg, cfg.APIKey)

	// Internal REST API on separate port (only exposed to Docker infra network)
	if cfg.RESTListenAddr != "" {
		rest := restapi.New(reg, cfg.APIKey)
		go func() {
			log.Printf("calendar-mcp REST API listening on %s (internal only)", cfg.RESTListenAddr)
			if err := http.ListenAndServe(cfg.RESTListenAddr, rest.Handler()); err != nil {
				log.Fatalf("REST API: %v", err)
			}
		}()
	}

	log.Printf("calendar-mcp MCP listening on %s (%d providers)", cfg.ListenAddr, len(providers))
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, mux))
}
