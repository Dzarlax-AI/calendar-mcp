package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/httpx"
)

// CachedEventReader is separate from the live application service so enabling
// cached summaries cannot change the established live tool contracts.
type CachedEventReader interface {
	Read(context.Context, cachedpages.Request, string) (cachedpages.Page, cachedpages.PageStats, error)
	Ready() bool
}

type CatalogMode string

const (
	CatalogCompat CatalogMode = "compat"
	CatalogModern CatalogMode = "modern"
)

type Options struct {
	EnableV2               bool
	CatalogMode            CatalogMode
	EventReadModelEnabled  bool
	GatewayCachedReadsOnly bool
	CachedEvents           CachedEventReader
}

// Register retains the original combined catalog at /mcp for existing callers.
func Register(mux *http.ServeMux, reg *calendar.Registry, app *application.Service, authOptions auth.Options, enableV2 bool) {
	_ = RegisterWithOptions(mux, reg, app, authOptions, Options{EnableV2: enableV2, CatalogMode: CatalogCompat})
}

func RegisterWithOptions(mux *http.ServeMux, reg *calendar.Registry, app *application.Service, authOptions auth.Options, opts Options) error {
	mode := opts.CatalogMode
	if mode == "" {
		mode = CatalogCompat
	}
	if mode != CatalogCompat && mode != CatalogModern {
		return fmt.Errorf("invalid MCP catalog mode %q", mode)
	}
	if mode == CatalogModern && !opts.EnableV2 {
		return fmt.Errorf("modern MCP catalog requires EnableV2")
	}

	compatHandler := streamableHandler(buildCompatCatalog(reg, app, opts.EnableV2))
	defaultHandler := compatHandler
	var modernHandler http.Handler
	if opts.EnableV2 {
		modern := buildModernCatalog(reg, app)
		cachedEnabled := opts.EventReadModelEnabled && opts.CachedEvents != nil && opts.CachedEvents.Ready() && authOptions.APIKey != ""
		if cachedEnabled {
			registerCachedEvents(modern, opts.CachedEvents)
		}
		modernHandler = streamableHandler(modern)
		if cachedEnabled {
			modernHandler = observeCachedRequestBody(modernHandler)
		}
		if mode == CatalogModern {
			defaultHandler = modernHandler
			if cachedEnabled {
				coverage, ok := opts.CachedEvents.(coverageEventReader)
				if !ok {
					return fmt.Errorf("modern default catalog requires the cached coverage reader")
				}
				main := newMCPServer()
				registerListCalendars(main, reg, "list_calendars")
				names := maps.Clone(modernToolNames)
				delete(names, toolGetEvents)
				registerToolsV2(main, app, names)
				registerCoverageEvents(main, opts.CachedEvents, coverage)
				defaultHandler = observeCachedRequestBody(streamableHandler(main))
			}
		}
	}

	if opts.GatewayCachedReadsOnly {
		reader, ok := opts.CachedEvents.(gatewayCatalogReader)
		if !ok || !reader.Ready() || !opts.EnableV2 || mode != CatalogModern || !opts.EventReadModelEnabled || authOptions.APIKey == "" {
			return fmt.Errorf("strict gateway catalog requires authenticated modern V2 and a local cache reader")
		}
		main := buildModernCatalog(reg, app)
		registerGatewayReads(main, reader)
		registerGatewayWriteTracking(main)
		defaultHandler = observeCachedRequestBody(streamableHandler(main))
	}

	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp", "/mcp/":
			defaultHandler.ServeHTTP(w, r)
		case "/mcp/compat", "/mcp/compat/":
			compatHandler.ServeHTTP(w, r)
		case "/mcp/modern", "/mcp/modern/":
			if modernHandler == nil {
				http.NotFound(w, r)
				return
			}
			modernHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	protected := auth.Middleware(authOptions)(httpx.LimitRequestBody(dispatch, httpx.DefaultMaxRequestBodyBytes))
	mux.Handle("/mcp", protected)
	mux.Handle("/mcp/", protected)
	return nil
}

func streamableHandler(s *server.MCPServer) http.Handler {
	return server.NewStreamableHTTPServer(s)
}

func buildCompatCatalog(reg *calendar.Registry, app *application.Service, enableV2 bool) *server.MCPServer {
	return buildServer(reg, app, enableV2)
}

func buildModernCatalog(reg *calendar.Registry, app *application.Service) *server.MCPServer {
	s := newMCPServer()
	registerListCalendars(s, reg, "list_calendars")
	registerToolsV2(s, app, modernToolNames)
	return s
}

func buildServer(reg *calendar.Registry, app *application.Service, enableV2 bool) *server.MCPServer {
	s := newMCPServer()
	registerTools(s, reg)
	if enableV2 {
		registerToolsV2(s, app, compatV2ToolNames)
	}
	return s
}

func newMCPServer() *server.MCPServer {
	return server.NewMCPServer("calendar-mcp", "1.0.0", server.WithToolCapabilities(true))
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}
