package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"

	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/google"
	"calendar-mcp/internal/mcpserver"
	"calendar-mcp/internal/microsoft"
	"calendar-mcp/internal/oauthflow"
	providerfactory "calendar-mcp/internal/providers"
	"calendar-mcp/internal/restapi"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/web"
)

// Serve starts the existing MCP and optional internal REST endpoints.
func Serve(_ context.Context) error {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	var platformStore *storage.Store
	var connectionService *connections.Service
	var credentialCipher *credentials.Cipher
	if cfg.DatabaseURL != "" {
		cipher, err := credentials.NewCipher(cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("credential encryption: %w", err)
		}
		credentialCipher = cipher
		platformStore, err = storage.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer platformStore.Close()
		if err := platformStore.Migrate(context.Background()); err != nil {
			return fmt.Errorf("migrate storage: %w", err)
		}
		platformStore.SetArtifactCipher(cipher)
		connectionService = connections.New(platformStore, cipher)
	}

	var calendarProviders []calendar.Provider
	if platformStore != nil {
		storedProviders, err := providerfactory.NewFactory(cfg, platformStore, connectionService).Build(context.Background())
		if err != nil {
			return fmt.Errorf("stored providers: %w", err)
		}
		calendarProviders = storedProviders
	} else if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		g, err := google.New(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRefreshToken, cfg.TokenDir)
		if err != nil {
			return fmt.Errorf("google provider: %w", err)
		}
		calendarProviders = append(calendarProviders, g)
		log.Println("google calendar provider enabled")
	}
	if platformStore == nil && cfg.MS365ClientID != "" && cfg.MS365ClientSecret != "" && cfg.MS365TenantID != "" {
		m, err := microsoft.New(cfg.MS365ClientID, cfg.MS365ClientSecret, cfg.MS365TenantID, cfg.MS365RefreshToken, cfg.TokenDir)
		if err != nil {
			return fmt.Errorf("microsoft provider: %w", err)
		}
		calendarProviders = append(calendarProviders, m)
		log.Println("microsoft calendar provider enabled")
	}
	if platformStore == nil && cfg.AppleUsername != "" && cfg.AppleAppPassword != "" {
		a, err := apple.New(cfg.AppleUsername, cfg.AppleAppPassword, cfg.AppleCalDAVURL)
		if err != nil {
			return fmt.Errorf("apple provider: %w", err)
		}
		calendarProviders = append(calendarProviders, a)
		log.Println("apple calendar provider enabled")
	}
	if platformStore == nil && len(calendarProviders) == 0 {
		return errors.New("no calendar providers configured")
	}

	reg := calendar.NewRegistry(calendarProviders, calendar.RegistryOptions{
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
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	apiAuth := auth.Options{
		APIKey:               cfg.APIKey,
		LegacyAPIKey:         cfg.LegacyAPIKey,
		AllowUnauthenticated: cfg.AllowUnauthenticated,
	}
	mcpserver.Register(mux, reg, appService, apiAuth, cfg.EnableV2)
	if platformStore != nil {
		publicURL := strings.TrimSuffix(cfg.PublicURL, "/")
		oauthProviders := map[string]oauthflow.Provider{}
		if cfg.GoogleClientID != "" {
			oauthProviders["google"] = oauthflow.ConfigProvider{Config: &oauth2.Config{
				ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret, Endpoint: googleoauth.Endpoint,
				RedirectURL: publicURL + "/oauth/google/callback", Scopes: google.OAuthScopes(),
			}, AuthorizationOptions: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("prompt", "consent")}}
		}
		if cfg.MS365ClientID != "" {
			base := "https://login.microsoftonline.com/" + cfg.MS365TenantID + "/oauth2/v2.0"
			oauthProviders["microsoft"] = oauthflow.ConfigProvider{Config: &oauth2.Config{
				ClientID: cfg.MS365ClientID, ClientSecret: cfg.MS365ClientSecret,
				Endpoint:    oauth2.Endpoint{AuthURL: base + "/authorize", TokenURL: base + "/token"},
				RedirectURL: publicURL + "/oauth/microsoft/callback", Scopes: []string{"https://graph.microsoft.com/Calendars.ReadWrite", "offline_access"},
			}}
		}
		factory := providerfactory.NewFactory(cfg, platformStore, connectionService)
		ui, err := web.New(platformStore, connectionService, oauthflow.New(platformStore, credentialCipher, oauthProviders), factory.Build, webRuntimeConfig(cfg, appService, reg.ReplaceProviders))
		if err != nil {
			return fmt.Errorf("initialize calendar UI: %w", err)
		}
		mux.Handle("/", ui.Handler())
	}
	servers := []*http.Server{NewHTTPServer(cfg.ListenAddr, mux, 0)}
	if cfg.RESTListenAddr != "" {
		rest := restapi.New(reg, appService, apiAuth, cfg.EnableV2)
		servers = append(servers, NewHTTPServer(cfg.RESTListenAddr, rest.Handler(), 2*time.Minute))
		log.Printf("calendar-mcp REST API listening on %s (internal only)", cfg.RESTListenAddr)
	}

	log.Printf("calendar-mcp MCP listening on %s (%d providers)", cfg.ListenAddr, len(calendarProviders))
	return RunServers(servers)
}

func webRuntimeConfig(cfg *config.Config, appService *application.Service, onProvidersChanged func([]calendar.Provider)) web.Config {
	return webRuntimeConfigAt(cfg, appService, onProvidersChanged, time.Now().UTC())
}

func webRuntimeConfigAt(cfg *config.Config, appService *application.Service, onProvidersChanged func([]calendar.Provider), anchor time.Time) web.Config {
	eventReadModelEnabled := cfg.EventReadModelEnabled
	window := storage.SyncWindow{}
	if eventReadModelEnabled {
		window = eventReadModelWindow(cfg, anchor)
	}
	return web.Config{
		PublicURL: cfg.PublicURL, TrustForwardAuth: cfg.TrustForwardAuth, AllowUnauthenticated: cfg.UIAllowUnauthenticated, RawArtifactOperators: cfg.UIRawArtifactUsers,
		MCPAPIKey: cfg.APIKey, LegacyAPIKeyConfigured: cfg.LegacyAPIKey != "",
		GoogleConfigured: cfg.GoogleClientID != "", MicrosoftConfigured: cfg.MS365ClientID != "", AppleCalDAVURL: cfg.AppleCalDAVURL,
		ApplicationService: appService, EventReadModelEnabled: &eventReadModelEnabled, EventReadModelWindow: window,
		OnProvidersChanged: onProvidersChanged,
	}
}

func NewHTTPServer(addr string, handler http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr: addr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: writeTimeout, IdleTimeout: time.Minute,
	}
}

func RunServers(servers []*http.Server) error {
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
