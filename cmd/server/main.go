package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/google"
	"calendar-mcp/internal/mcpserver"
	"calendar-mcp/internal/microsoft"
	"calendar-mcp/internal/restapi"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("configuration: %v", err)
	}

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
	appService := application.New(reg)
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
	mcpserver.Register(mux, reg, appService, cfg.APIKey, cfg.AllowUnauthenticated, cfg.EnableV2)
	servers := []*http.Server{newHTTPServer(cfg.ListenAddr, mux, 0)}

	// Internal REST API on separate port (only exposed to Docker infra network)
	if cfg.RESTListenAddr != "" {
		rest := restapi.New(reg, appService, cfg.APIKey, cfg.AllowUnauthenticated, cfg.EnableV2)
		servers = append(servers, newHTTPServer(cfg.RESTListenAddr, rest.Handler(), 2*time.Minute))
		log.Printf("calendar-mcp REST API listening on %s (internal only)", cfg.RESTListenAddr)
	}

	log.Printf("calendar-mcp MCP listening on %s (%d providers)", cfg.ListenAddr, len(providers))
	if err := runServers(servers); err != nil {
		log.Fatal(err)
	}
}

func newHTTPServer(addr string, handler http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       time.Minute,
	}
}

func runServers(servers []*http.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, len(servers))
	for _, server := range servers {
		server := server
		go func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	var serveErrs []error
	select {
	case err := <-errCh:
		serveErrs = append(serveErrs, err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var shutdownErrs []error
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			shutdownErrs = append(shutdownErrs, err)
		}
	}
	for {
		select {
		case err := <-errCh:
			serveErrs = append(serveErrs, err)
		default:
			goto errorsCollected
		}
	}

errorsCollected:
	var resultErrs []error
	if len(serveErrs) > 0 {
		resultErrs = append(resultErrs, fmt.Errorf("serve HTTP: %w", errors.Join(serveErrs...)))
	}
	if len(shutdownErrs) > 0 {
		resultErrs = append(resultErrs, fmt.Errorf("server shutdown: %w", errors.Join(shutdownErrs...)))
	}
	return errors.Join(resultErrs...)
}
