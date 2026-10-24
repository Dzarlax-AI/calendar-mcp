package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
)

func TestCatalogStreamableHTTPClients(t *testing.T) {
	for _, test := range []struct {
		name string
		opts Options
	}{
		{name: "default compatibility", opts: Options{EnableV2: true}},
		{name: "modern default", opts: Options{EnableV2: true, CatalogMode: CatalogModern}},
		{name: "original tools only", opts: Options{CatalogMode: CatalogCompat}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := calendar.NewRegistry(nil)
			mux := http.NewServeMux()
			if err := RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "primary", LegacyAPIKey: "legacy"}, test.opts); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(mux)
			defer server.Close()
			for _, path := range []string{"/mcp", "/mcp/", "/mcp/compat", "/mcp/compat/", "/mcp/modern", "/mcp/modern/"} {
				t.Run(path, func(t *testing.T) {
					modern := strings.HasPrefix(path, "/mcp/modern") ||
						(strings.TrimSuffix(path, "/") == "/mcp" && test.opts.CatalogMode == CatalogModern)
					if modern && !test.opts.EnableV2 {
						request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
						if err != nil {
							t.Fatal(err)
						}
						request.Header.Set("Authorization", "Bearer primary")
						response, err := server.Client().Do(request)
						if err != nil {
							t.Fatal(err)
						}
						defer response.Body.Close()
						if response.StatusCode != http.StatusNotFound {
							t.Fatalf("disabled modern status = %d, want 404", response.StatusCode)
						}
						return
					}
					// Exercise both supported credentials over the actual HTTP transport.
					token := "primary"
					if strings.HasSuffix(path, "/") {
						token = "legacy"
					}
					c, err := client.NewStreamableHttpClient(server.URL+path,
						transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					defer c.Close()
					if err := c.Start(ctx); err != nil {
						t.Fatal(err)
					}
					initialize := mcp.InitializeRequest{}
					initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
					initialize.Params.ClientInfo = mcp.Implementation{Name: "catalog-http-test", Version: "1"}
					if _, err := c.Initialize(ctx, initialize); err != nil {
						t.Fatal(err)
					}
					listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
					if err != nil {
						t.Fatal(err)
					}
					wantCount := 5
					if modern {
						wantCount = 12
					} else if test.opts.EnableV2 {
						wantCount = 16
					}
					if len(listed.Tools) != wantCount {
						t.Fatalf("catalog has %d tools, want %d", len(listed.Tools), wantCount)
					}
					for _, tool := range listed.Tools {
						if modern && strings.HasSuffix(tool.Name, "_v2") {
							t.Fatalf("modern endpoint exposes %q", tool.Name)
						}
					}
					call := mcp.CallToolRequest{}
					call.Params.Name = "list_calendars"
					call.Params.Arguments = map[string]any{}
					result, err := c.CallTool(ctx, call)
					if err != nil || result == nil || result.IsError {
						t.Fatalf("shared calendar read = %+v, error %v", result, err)
					}
					if modern {
						call.Params.Name = "get_events_v2"
						result, err = c.CallTool(ctx, call)
						if err == nil && (result == nil || !result.IsError) {
							t.Fatalf("modern accepted compatibility name: %+v", result)
						}
					}
				})
			}
		})
	}
}
