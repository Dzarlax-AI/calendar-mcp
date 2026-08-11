package mcpserver

import (
	"encoding/json"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/httpx"
)

func Register(mux *http.ServeMux, reg *calendar.Registry, app *application.Service, apiKey string, allowUnauthenticated, enableV2 bool) {
	s := buildServer(reg, app, enableV2)
	h := server.NewStreamableHTTPServer(s)
	limited := httpx.LimitRequestBody(h, httpx.DefaultMaxRequestBodyBytes)
	protected := auth.Middleware(auth.Options{APIKey: apiKey, AllowUnauthenticated: allowUnauthenticated})(limited)
	mux.Handle("/mcp", protected)
	mux.Handle("/mcp/", protected)
}

func buildServer(reg *calendar.Registry, app *application.Service, enableV2 bool) *server.MCPServer {
	s := server.NewMCPServer("calendar-mcp", "1.0.0",
		server.WithToolCapabilities(true),
	)
	registerTools(s, reg)
	if enableV2 {
		registerToolsV2(s, app)
	}
	return s
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}
