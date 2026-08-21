package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"

	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/oauthflow"
	"calendar-mcp/internal/storage"
)

//go:embed templates/*.html assets/* legal/*.md
var content embed.FS

type ProviderBuilder func(context.Context) ([]calendar.Provider, error)

type Config struct {
	PublicURL            string
	TrustForwardAuth     bool
	AllowUnauthenticated bool
	GoogleConfigured     bool
	MicrosoftConfigured  bool
	AppleCalDAVURL       string
	OnProvidersChanged   func([]calendar.Provider)
}

type Server struct {
	store       *storage.Store
	connections *connections.Service
	oauth       *oauthflow.Service
	providers   ProviderBuilder
	config      Config
	template    *template.Template
	publicDocs  map[string]template.HTML
	origin      string
}

func New(store *storage.Store, connectionService *connections.Service, oauthService *oauthflow.Service, providers ProviderBuilder, cfg Config) (*Server, error) {
	parsed, err := template.New("app.html").ParseFS(content, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse UI templates: %w", err)
	}
	publicDocs := make(map[string]template.HTML, 2)
	for _, name := range []string{"privacy", "terms"} {
		source, err := content.ReadFile("legal/" + name + ".md")
		if err != nil {
			return nil, fmt.Errorf("read %s document: %w", name, err)
		}
		rendered, err := renderMarkdown(source)
		if err != nil {
			return nil, fmt.Errorf("render %s document: %w", name, err)
		}
		publicDocs[name] = rendered
	}
	origin := ""
	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, errors.New("CALENDAR_PUBLIC_URL must be an absolute URL")
		}
		origin = u.Scheme + "://" + u.Host
	}
	return &Server{store: store, connections: connectionService, oauth: oauthService, providers: providers, config: cfg, template: parsed, publicDocs: publicDocs, origin: origin}, nil
}

func renderMarkdown(source []byte) (template.HTML, error) {
	var rendered bytes.Buffer
	if err := goldmark.New().Convert(source, &rendered); err != nil {
		return "", err
	}
	// Goldmark omits raw HTML and dangerous link destinations by default. The
	// result is generated only from repository-controlled Markdown.
	return template.HTML(rendered.String()), nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(content, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.Handle("GET /{$}", http.HandlerFunc(s.publicPage("home")))
	mux.Handle("GET /privacy", http.HandlerFunc(s.publicPage("privacy")))
	mux.Handle("GET /terms", http.HandlerFunc(s.publicPage("terms")))
	mux.Handle("GET /oauth/{provider}/callback", http.HandlerFunc(s.oauthCallback))
	mux.Handle("GET /app", s.protected(http.HandlerFunc(s.page("dashboard"))))
	mux.Handle("GET /connections", s.protected(http.HandlerFunc(s.page("connections"))))
	mux.Handle("GET /rules", s.protected(http.HandlerFunc(s.page("rules"))))
	mux.Handle("GET /rules/new", s.protected(http.HandlerFunc(s.page("new-rule"))))
	mux.Handle("GET /runs", s.protected(http.HandlerFunc(s.page("runs"))))
	mux.Handle("GET /settings", s.protected(http.HandlerFunc(s.page("settings"))))
	mux.Handle("GET /oauth/{provider}/start", s.protected(http.HandlerFunc(s.oauthStart)))
	mux.Handle("GET /connections/{id}/oauth/{provider}/start", s.protected(http.HandlerFunc(s.oauthStart)))
	mux.Handle("POST /connections/apple", s.protected(s.mutating(http.HandlerFunc(s.connectApple))))
	mux.Handle("POST /connections/{id}/delete", s.protected(s.mutating(http.HandlerFunc(s.deleteConnection))))
	mux.Handle("POST /rules", s.protected(s.mutating(http.HandlerFunc(s.createRule))))
	mux.Handle("POST /rules/{id}/enable", s.protected(s.mutating(http.HandlerFunc(s.enableRule))))
	mux.Handle("POST /rules/{id}/run", s.protected(s.mutating(http.HandlerFunc(s.runRule))))
	return mux
}

type publicViewData struct {
	Title, Description, Page string
	Content                  template.HTML
}

func (s *Server) publicPage(page string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		data := publicViewData{Page: page}
		switch page {
		case "home":
			data.Title = "Calendar Platform"
			data.Description = "Connect Google, Microsoft, and Apple calendars, create safe one-way sync rules, and use one calendar backend from MCP clients."
		case "privacy":
			data.Title = "Privacy Policy"
			data.Description = "How Calendar Platform accesses, uses, stores, and shares calendar data."
			data.Content = s.publicDocs[page]
		case "terms":
			data.Title = "Terms of Service"
			data.Description = "Terms for using the hosted Calendar Platform service and self-hosted software."
			data.Content = s.publicDocs[page]
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := s.template.ExecuteTemplate(w, "public.html", data); err != nil {
			log.Printf("calendar public page template: %v", err)
		}
	}
}

type providerCard struct {
	Provider, Name, Icon, Help string
	Connected                  bool
	ConnectionCount            int
	CalendarCount              int
}
type connectionView struct {
	ID, Provider, DisplayName, Status string
	CalendarCount                     int
}
type ruleView struct {
	ID, Source, Target, State                    string
	SourceProvider, SourceIcon                   string
	TargetProvider, TargetIcon                   string
	IntervalMinutes, LookbackDays, LookaheadDays int
}
type settingView struct {
	Name, Description string
	Ready             bool
}
type calendarOption struct {
	ID, Label string
	Selected  bool
}
type viewData struct {
	Title, Page, Flash, CSRFToken    string
	Connections                      []storage.Connection
	ConnectionViews                  []connectionView
	Calendars                        []storage.Calendar
	SourceCalendars, TargetCalendars []calendarOption
	Rules                            []storage.Rule
	Runs                             []storage.Run
	ProviderCards                    []providerCard
	RuleViews                        []ruleView
	Settings                         []settingView
	Attention                        int
}

func (s *Server) page(page string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" && page == "dashboard" {
			http.NotFound(w, r)
			return
		}
		data, err := s.viewData(r.Context(), page, csrfToken(w, r))
		if err != nil {
			log.Printf("calendar UI page data: %v", err)
			http.Error(w, "Calendar UI is temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		data.Flash = flashMessage(r.URL.Query().Get("status"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.template.ExecuteTemplate(w, "app.html", data); err != nil {
			log.Printf("calendar UI template: %v", err)
		}
	}
}

func (s *Server) viewData(ctx context.Context, page, csrf string) (viewData, error) {
	connectionsList, err := s.store.ListConnections(ctx)
	if err != nil {
		return viewData{}, err
	}
	calendars, err := s.store.ListAllCalendars(ctx)
	if err != nil {
		return viewData{}, err
	}
	rules, err := s.store.ListRules(ctx)
	if err != nil {
		return viewData{}, err
	}
	runs, err := s.store.ListRuns(ctx, 50)
	if err != nil {
		return viewData{}, err
	}
	data := viewData{Title: pageTitle(page), Page: page, CSRFToken: csrf, Connections: connectionsList, Calendars: calendars, Rules: rules, Runs: runs}
	connectionCounts := map[string]int{}
	providerCalendarCounts := map[string]int{}
	calendarNames := map[string]string{}
	connectionNames := map[string]string{}
	for _, connection := range connectionsList {
		connectionCounts[connection.Provider]++
		connectionNames[connection.ID] = connection.DisplayName
		if connection.Status == "error" {
			data.Attention++
		}
	}
	for _, discovered := range calendars {
		for _, connection := range connectionsList {
			if connection.ID == discovered.ConnectionID {
				providerCalendarCounts[connection.Provider]++
				break
			}
		}
		calendarNames[discovered.ID] = connectionNames[discovered.ConnectionID] + " / " + discovered.Name
	}
	for _, connection := range connectionsList {
		count := 0
		for _, discovered := range calendars {
			if discovered.ConnectionID == connection.ID {
				count++
			}
		}
		data.ConnectionViews = append(data.ConnectionViews, connectionView{ID: connection.ID, Provider: connection.Provider, DisplayName: connection.DisplayName, Status: connection.Status, CalendarCount: count})
	}
	for _, spec := range []providerCard{{Provider: "google", Name: "Google Calendar", Icon: "G", Help: "Connect with OAuth and grant calendar access."}, {Provider: "microsoft", Name: "Microsoft 365", Icon: "MS", Help: "Connect with OAuth and grant Microsoft Graph calendar access."}, {Provider: "apple", Name: "Apple Calendar", Icon: "A", Help: "Connect with an Apple identifier and app-specific password."}} {
		if connectionCounts[spec.Provider] > 0 {
			spec.Connected = true
			spec.ConnectionCount = connectionCounts[spec.Provider]
			spec.CalendarCount = providerCalendarCounts[spec.Provider]
		}
		data.ProviderCards = append(data.ProviderCards, spec)
	}
	for _, rule := range rules {
		sourceProvider := calendarProviderName(rule.SourceCalendarID)
		targetProvider := calendarProviderName(rule.TargetCalendarID)
		data.RuleViews = append(data.RuleViews, ruleView{ID: rule.ID, Source: calendarNames[rule.SourceCalendarID], Target: calendarNames[rule.TargetCalendarID], State: rule.State, SourceProvider: sourceProvider, SourceIcon: providerIcon(sourceProvider), TargetProvider: targetProvider, TargetIcon: providerIcon(targetProvider), IntervalMinutes: rule.IntervalSeconds / 60, LookbackDays: rule.LookbackDays, LookaheadDays: rule.LookaheadDays})
	}
	for _, discovered := range calendars {
		option := calendarOption{ID: discovered.ID, Label: connectionNames[discovered.ConnectionID] + " / " + discovered.Name}
		if discovered.CanRead {
			data.SourceCalendars = append(data.SourceCalendars, option)
		}
		if discovered.CanWrite {
			data.TargetCalendars = append(data.TargetCalendars, option)
		}
	}
	if len(data.SourceCalendars) > 0 {
		data.SourceCalendars[0].Selected = true
	}
	if len(data.TargetCalendars) > 0 {
		defaultTarget := 0
		if len(data.TargetCalendars) > 1 && len(data.SourceCalendars) > 0 && data.TargetCalendars[0].ID == data.SourceCalendars[0].ID {
			defaultTarget = 1
		}
		data.TargetCalendars[defaultTarget].Selected = true
	}
	data.Settings = []settingView{{"Database", "PostgreSQL recommended; SQLite supported for self-hosters.", true}, {"Credential encryption", "Application-level authenticated encryption.", true}, {"Google OAuth application", "Client configuration supplied through environment variables.", s.config.GoogleConfigured}, {"Microsoft OAuth application", "Tenant and client configuration supplied through environment variables.", s.config.MicrosoftConfigured}}
	return data, nil
}

func calendarProviderName(calendarID string) string {
	prefix, _, _ := strings.Cut(calendarID, ":")
	provider, _, _ := strings.Cut(prefix, "@")
	return provider
}

func providerIcon(provider string) string {
	switch provider {
	case "google":
		return "G"
	case "microsoft":
		return "MS"
	case "apple":
		return "A"
	default:
		return "?"
	}
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	connectionID := r.PathValue("id")
	var start oauthflow.Start
	var err error
	if connectionID == "" {
		start, err = s.oauth.Begin(r.Context(), provider, r.URL.Query().Get("return"))
	} else {
		connection, loadErr := s.store.ConnectionByID(r.Context(), connectionID)
		if loadErr != nil || connection.Provider != provider || provider == "apple" {
			http.Error(w, "Connection not found", http.StatusNotFound)
			return
		}
		start, err = s.oauth.BeginReconnect(r.Context(), provider, connectionID, r.URL.Query().Get("return"))
	}
	if err != nil {
		log.Printf("start %s OAuth: %v", provider, err)
		http.Redirect(w, r, "/connections?status=oauth_start_failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, start.AuthorizationURL, http.StatusFound)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if r.URL.Query().Get("error") != "" {
		http.Redirect(w, r, "/connections?status=oauth_rejected", http.StatusSeeOther)
		return
	}
	completion, err := s.oauth.CompleteWithTarget(r.Context(), provider, r.URL.Query().Get("state"), r.URL.Query().Get("code"))
	if err != nil {
		log.Printf("complete %s OAuth: %v", provider, err)
		http.Redirect(w, r, "/connections?status=oauth_failed", http.StatusSeeOther)
		return
	}
	labels := map[string]string{"google": "Google Calendar", "microsoft": "Microsoft 365"}
	id := completion.ConnectionID
	if completion.Mode == "reconnect" {
		err = s.connections.ReconnectOAuth(r.Context(), id, provider, completion.Token)
	} else {
		id, err = s.connections.ConnectOAuth(r.Context(), provider, labels[provider], completion.Token, nil)
	}
	if err == nil {
		err = s.verifyConnection(r.Context(), id, provider)
	}
	if err != nil {
		log.Printf("save or verify %s connection: %v", provider, err)
		http.Redirect(w, r, "/connections?status=verification_failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, completion.ReturnPath+"?status=connected", http.StatusSeeOther)
}

func (s *Server) connectApple(w http.ResponseWriter, r *http.Request) {
	username, password := r.FormValue("username"), r.FormValue("app_password")
	provider, err := apple.New(username, password, s.config.AppleCalDAVURL)
	if err == nil {
		_, err = provider.ListCalendars(r.Context())
	}
	if err != nil {
		log.Printf("verify Apple connection failed: category=%T", err)
		http.Redirect(w, r, "/connections?status=verification_failed", http.StatusSeeOther)
		return
	}
	id := r.FormValue("connection_id")
	if id == "" {
		id, err = s.connections.ConnectApple(r.Context(), "Apple Calendar", username, password)
	} else {
		err = s.connections.ReconnectApple(r.Context(), id, username, password)
	}
	if err == nil {
		err = s.connections.VerifyAndDiscover(r.Context(), id, provider)
	}
	if err == nil {
		err = s.refreshProviders(r.Context())
	}
	if err != nil {
		log.Printf("save Apple connection: %v", err)
		http.Redirect(w, r, "/connections?status=connection_failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/connections?status=connected", http.StatusSeeOther)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if err := s.connections.Delete(r.Context(), r.PathValue("id")); err != nil {
		log.Printf("delete connection: %v", err)
		status := "connection_delete_failed"
		if errors.Is(err, storage.ErrConnectionInUse) {
			status = "connection_in_use"
		}
		http.Redirect(w, r, "/connections?status="+status, http.StatusSeeOther)
		return
	}
	if err := s.refreshProviders(r.Context()); err != nil {
		log.Printf("refresh providers after connection delete: %v", err)
	}
	http.Redirect(w, r, "/connections?status=connection_deleted", http.StatusSeeOther)
}

func (s *Server) verifyConnection(ctx context.Context, id, name string) error {
	providers, err := s.providers(ctx)
	if err != nil {
		return err
	}
	for _, provider := range providers {
		if provider.Name() == name && calendar.ProviderRouteName(provider) == name+"@"+id {
			if err := s.connections.VerifyAndDiscover(ctx, id, provider); err != nil {
				return err
			}
			return s.refreshProviders(ctx)
		}
	}
	return errors.New("connected provider was not built")
}

func (s *Server) refreshProviders(ctx context.Context) error {
	refreshed, err := s.providers(ctx)
	if err != nil {
		return err
	}
	if s.config.OnProvidersChanged != nil {
		s.config.OnProvidersChanged(refreshed)
	}
	return nil
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	interval, intervalErr := strconv.Atoi(r.FormValue("interval_seconds"))
	lookback, lookbackErr := strconv.Atoi(r.FormValue("lookback_days"))
	lookahead, lookaheadErr := strconv.Atoi(r.FormValue("lookahead_days"))
	if intervalErr != nil || lookbackErr != nil || lookaheadErr != nil {
		http.Redirect(w, r, "/rules/new?status=invalid_rule", http.StatusSeeOther)
		return
	}
	now := time.Now().UTC()
	rule := storage.Rule{ID: newID(), SourceCalendarID: r.FormValue("source_calendar_id"), TargetCalendarID: r.FormValue("target_calendar_id"), State: "paused", IntervalSeconds: interval, LookbackDays: lookback, LookaheadDays: lookahead, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateRule(r.Context(), rule); err != nil {
		log.Printf("create sync rule: %v", err)
		http.Redirect(w, r, "/rules/new?status=invalid_rule", http.StatusSeeOther)
		return
	}
	job := storage.Job{ID: newID(), RuleID: rule.ID, Kind: "dry_run", State: "pending", AvailableAt: now, CreatedAt: now}
	if err := s.store.EnqueueJob(r.Context(), job); err != nil {
		log.Printf("enqueue dry run: %v", err)
		http.Redirect(w, r, "/rules?status=queue_failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/rules?status=rule_created", http.StatusSeeOther)
}

func (s *Server) enableRule(w http.ResponseWriter, r *http.Request) {
	ok, err := s.store.HasSuccessfulDryRun(r.Context(), r.PathValue("id"))
	if err != nil {
		log.Printf("check successful dry run: %v", err)
		http.Redirect(w, r, "/rules?status=queue_failed", http.StatusSeeOther)
		return
	}
	if !ok {
		http.Redirect(w, r, "/rules?status=dry_run_required", http.StatusSeeOther)
		return
	}
	now := time.Now().UTC()
	if err := s.store.SetRuleState(r.Context(), r.PathValue("id"), "enabled", &now, now); err != nil {
		log.Printf("enable sync rule: %v", err)
		http.Redirect(w, r, "/rules?status=invalid_rule", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/rules?status=rule_enabled", http.StatusSeeOther)
}

func (s *Server) runRule(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	kind := r.FormValue("kind")
	if kind != "dry_run" {
		kind = "manual"
	}
	job := storage.Job{ID: newID(), RuleID: r.PathValue("id"), Kind: kind, State: "pending", AvailableAt: now, CreatedAt: now}
	if err := s.store.EnqueueJob(r.Context(), job); err != nil {
		log.Printf("enqueue sync rule: %v", err)
		http.Redirect(w, r, "/rules?status=queue_failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/runs?status=run_queued", http.StatusSeeOther)
}

func (s *Server) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (s.config.TrustForwardAuth && r.Header.Get("X-authentik-username") == "") || (!s.config.TrustForwardAuth && !s.config.AllowUnauthenticated) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) mutating(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.origin == "" || r.Header.Get("Origin") != s.origin {
			http.Error(w, "Invalid request origin", http.StatusForbidden)
			return
		}
		cookie, err := r.Cookie("calendar_csrf")
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.FormValue("csrf_token"))) != 1 {
			http.Error(w, "Invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func csrfToken(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie("calendar_csrf"); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	token := base64.RawURLEncoding.EncodeToString(value)
	http.SetCookie(w, &http.Cookie{Name: "calendar_csrf", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	return token
}

func newID() string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}
func pageTitle(page string) string {
	titles := map[string]string{"dashboard": "Dashboard", "connections": "Connections", "rules": "Sync Rules", "new-rule": "New Sync Rule", "runs": "Runs", "settings": "Settings"}
	return titles[page]
}
func flashMessage(code string) string {
	messages := map[string]string{"connected": "Connection verified and calendars discovered.", "connection_deleted": "Connection deleted.", "connection_in_use": "This connection is used by a sync rule and cannot be deleted.", "connection_delete_failed": "The connection could not be deleted.", "rule_created": "Rule saved paused and a dry run was queued.", "rule_enabled": "Rule enabled.", "run_queued": "Run queued.", "dry_run_required": "A successful dry run is required before enablement.", "oauth_rejected": "The provider rejected the authorization request.", "oauth_failed": "Authorization could not be completed. Start the connection again.", "verification_failed": "Credentials were accepted but calendar access could not be verified.", "invalid_rule": "The sync rule settings are invalid.", "queue_failed": "The rule was saved, but its dry run could not be queued."}
	return messages[code]
}
