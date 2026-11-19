package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/httpx"
)

type CachedEventReader interface {
	Read(context.Context, cachedpages.Request, string) (cachedpages.Page, cachedpages.PageStats, error)
	Ready() bool
}

// Register mounts the authenticated, platform-backed Calendar MCP endpoint.
func Register(mux *http.ServeMux, app *application.Service, authOptions auth.Options, reader gatewayCatalogReader) error {
	if reader == nil || !reader.Ready() || authOptions.APIKey == "" {
		return fmt.Errorf("calendar MCP requires a primary API key and a ready local cache reader")
	}
	catalog := newMCPServer()
	registerToolsV2(catalog, app, catalogToolNames)
	registerGatewayReads(catalog, reader)
	registerGatewayWriteTracking(catalog)
	handler := observeCachedRequestBody(streamableHandler(catalog))
	protected := auth.Middleware(authOptions)(httpx.LimitRequestBody(handler, httpx.DefaultMaxRequestBodyBytes))
	mux.Handle("/mcp", protected)
	mux.Handle("/mcp/{$}", protected)
	mux.Handle("/mcp/", http.NotFoundHandler())
	return nil
}

func streamableHandler(s *server.MCPServer) http.Handler {
	return server.NewStreamableHTTPServer(s)
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
