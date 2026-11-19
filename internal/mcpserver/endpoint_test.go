package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type endpointReader struct{ gatewayCatalogReader }

func (endpointReader) Ready() bool { return true }

func (endpointReader) GatewayListCalendars(context.Context) ([]calendar.Calendar, error) {
	return []calendar.Calendar{{ID: "google:fixture:calendar", Provider: "google"}}, nil
}

func TestSingleMCPEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	app := application.New(calendar.NewRegistry(nil))
	if err := Register(mux, app, auth.Options{APIKey: "primary", LegacyAPIKey: "previous"}, endpointReader{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mcp/compat", "/mcp/compat/", "/mcp/modern", "/mcp/modern/", "/mcp/anything"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want 404", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /mcp status = %d", response.Code)
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	want := []string{"create_event", "delete_event", "get_calendar_capabilities", "get_event", "get_event_instances",
		"get_events", "import_event", "list_calendars", "move_event", "respond_to_event", "search_events", "update_event"}
	for _, tc := range []struct{ path, key string }{{"/mcp", "primary"}, {"/mcp/", "previous"}} {
		t.Run(tc.path, func(t *testing.T) {
			c, err := client.NewStreamableHttpClient(server.URL+tc.path,
				transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + tc.key}))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx := t.Context()
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
			names := make([]string, 0, len(listed.Tools))
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, want) {
				t.Fatalf("listed tools = %v, want %v", names, want)
			}
			request := mcp.CallToolRequest{}
			request.Params.Name = "list_calendars"
			out, err := c.CallTool(ctx, request)
			if err != nil || out.IsError || len(out.Content) != 1 {
				t.Fatalf("list_calendars result=%+v err=%v", out, err)
			}
			if item, ok := out.Content[0].(mcp.TextContent); !ok || !strings.Contains(item.Text, "google:fixture:calendar") {
				t.Fatal("list_calendars did not use the local reader")
			}
		})
	}
}

func TestMCPRequiresReadyReaderAndPrimaryKey(t *testing.T) {
	app := application.New(calendar.NewRegistry(nil))
	if err := Register(http.NewServeMux(), app, auth.Options{APIKey: "primary"}, nil); err == nil {
		t.Fatal("registered MCP without a cache reader")
	}
	if err := Register(http.NewServeMux(), app, auth.Options{AllowUnauthenticated: true}, endpointReader{}); err == nil {
		t.Fatal("registered MCP without a primary key")
	}
}
