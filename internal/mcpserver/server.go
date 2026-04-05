package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"calendar-mcp/internal/calendar"
)

func Register(mux *http.ServeMux, reg *calendar.Registry, apiKey string) {
	s := buildServer(reg)
	h := server.NewStreamableHTTPServer(s)
	protected := withAPIKey(h, apiKey)
	mux.Handle("/mcp", protected)
	mux.Handle("/mcp/", protected)
}

func withAPIKey(next http.Handler, apiKey string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		key := strings.TrimPrefix(auth, "Bearer ")
		if key == "" {
			key = r.Header.Get("X-API-Key")
		}
		if key != apiKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func buildServer(reg *calendar.Registry) *server.MCPServer {
	s := server.NewMCPServer("calendar-mcp", "1.0.0",
		server.WithToolCapabilities(true),
	)
	registerTools(s, reg)
	return s
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}
