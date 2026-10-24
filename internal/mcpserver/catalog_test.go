package mcpserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
)

// These hashes were captured from the pre-catalog registration on 2026-10-24.
// They include each tool's name, description, annotations, and full input schema.
var compatToolSnapshots = map[string]string{
	"list_calendars":            "4a14591ebd05097b5b7b84da5f103375c058d62cc0d962584d5a3ecdf78313f9",
	"get_events":                "742c58299ccd2778e44a1b4df2b8b5316d166fc1dfc6cc7b6e280f852f48f01e",
	"create_event":              "a81680d4943af4fa7bac6128dd4d80650fca5b4edd103225355a0b394966f515",
	"update_event":              "3ff6a385ff6c6776351a7ecda8854a015f8fe923ce703bcd14215117f22110ee",
	"delete_event":              "a4aef2926ddc414397e949820c56f0b37155b49f31ecd6254a7e22ba16c6cef3",
	"get_calendar_capabilities": "b8752c4d34364c4e056ee76a9fc93521ca421b6197a0616eb87aed25ded7484a",
	"get_events_v2":             "8eb430411f17defd8f844ca5a540e6b2e05fa7180929d1641f10dd2b3c0683e5",
	"get_event_v2":              "3ae242e1034db46b739fe353b57a641947dabc242348f4c3611cc0216dff54ee",
	"create_event_v2":           "4ba20f6bd3b04d9ce67a1638ff537ee958a1868694f3dfddd8ad4d6735d5be34",
	"update_event_v2":           "afa489e1470003c3e3806360edb37a8535e9522a86b6c5b2912d2a5b09b09009",
	"delete_event_v2":           "41e0f6fe17ef98737eeb1c4650677c97a1af8fb611848d7dae453fe091b99429",
	"get_event_instances_v2":    "2ef8f77a26d9ac003c4a2cf2ef099fe9ad44b2922f446fe4936f8986d4feb55a",
	"search_events_v2":          "6c12629488f2691cfe3178b513dffe2170a3c6d8ec7cf62c61ba5808400b1032",
	"respond_to_event_v2":       "f68d3e6d171e525e2da7cc4ee06e58e8213806bcbb756d2767434ac2c269301f",
	"move_event_v2":             "a5557dde862c16e38f9648f8c0e9893d752ef22ea72e94c0612a1185449dbc8a",
	"import_event_v2":           "ef53e94be18366aa0956e29bd814edc3035e801079f1423e48751efbda1b2884",
}

func TestCatalogToolSetsAndCompatibilitySnapshots(t *testing.T) {
	reg := calendar.NewRegistry(nil)
	app := application.New(reg)
	compat := buildServer(reg, app, true)
	if got := compat.ListTools(); len(got) != 16 {
		t.Fatalf("compat catalog has %d tools, want 16", len(got))
	}
	for name, want := range compatToolSnapshots {
		tool := compat.GetTool(name)
		if tool == nil {
			t.Fatalf("compat tool %q missing", name)
		}
		if got := toolSnapshotHash(t, tool.Tool); got != want {
			t.Errorf("compat tool %q snapshot changed: got %s want %s", name, got, want)
		}
	}
	if compat.GetTool("no_such_tool") != nil {
		t.Fatal("unknown compat tool unexpectedly exists")
	}

	modern := buildModernCatalog(reg, app)
	if got := modern.ListTools(); len(got) != 12 {
		t.Fatalf("modern catalog has %d tools, want 12", len(got))
	}
	for _, name := range []string{"list_calendars", "get_calendar_capabilities", "get_events", "get_event", "create_event", "update_event", "delete_event", "get_event_instances", "search_events", "respond_to_event", "move_event", "import_event"} {
		if modern.GetTool(name) == nil {
			t.Errorf("modern tool %q missing", name)
		}
	}
	for _, oldName := range []string{"get_events_v2", "get_event_v2", "create_event_v2", "update_event_v2", "delete_event_v2", "get_event_instances_v2", "search_events_v2", "respond_to_event_v2", "move_event_v2", "import_event_v2"} {
		if modern.GetTool(oldName) != nil {
			t.Errorf("modern catalog exposes old tool name %q", oldName)
		}
	}
	for key, oldName := range compatV2ToolNames {
		modernTool := modern.GetTool(modernToolNames[key])
		compatTool := compat.GetTool(oldName)
		if string(modernTool.Tool.RawInputSchema) != string(compatTool.Tool.RawInputSchema) {
			t.Errorf("modern %q schema differs from compat %q", modernTool.Tool.Name, oldName)
		}
		if modernTool.Tool.Description != compatTool.Tool.Description {
			t.Errorf("modern %q description differs from compat %q", modernTool.Tool.Name, oldName)
		}
	}
	if got := buildServer(reg, app, false).ListTools(); len(got) != 5 {
		t.Fatalf("compat catalog without V2 has %d tools, want 5", len(got))
	}
}

func toolSnapshotHash(t *testing.T, tool mcp.Tool) string {
	t.Helper()
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func TestRegisterWithOptionsRoutesCatalogsAndProtectsPaths(t *testing.T) {
	reg := calendar.NewRegistry(nil)
	app := application.New(reg)
	mux := http.NewServeMux()
	err := RegisterWithOptions(mux, reg, app, auth.Options{APIKey: "primary", LegacyAPIKey: "legacy"}, Options{EnableV2: true, CatalogMode: CatalogModern})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://catalog.test"

	for _, path := range []string{"/mcp", "/mcp/", "/mcp/modern", "/mcp/modern/", "/mcp/compat", "/mcp/compat/"} {
		for _, token := range []string{"primary", "legacy"} {
			req, _ := http.NewRequest(http.MethodOptions, endpoint+path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp := serveDirect(mux, req)
			if resp.StatusCode == http.StatusUnauthorized {
				t.Errorf("%s rejected %s token", path, token)
			}
		}
	}
	for _, path := range []string{"/mcp", "/mcp/compat", "/mcp/modern", "/mcp/unknown"} {
		req, _ := http.NewRequest(http.MethodGet, endpoint+path, nil)
		resp := serveDirect(mux, req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s status = %d, want 401", path, resp.StatusCode)
		}
	}
	for _, invalid := range []string{"wrong", "native-app-token", "read-only-token"} {
		req, _ := http.NewRequest(http.MethodOptions, endpoint+"/mcp/modern", nil)
		req.Header.Set("Authorization", "Bearer "+invalid)
		resp := serveDirect(mux, req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("invalid token %q status = %d, want 401", invalid, resp.StatusCode)
		}
	}
	for _, path := range []string{"/mcp/compat/extra", "/mcp/modern/extra", "/mcp/unknown"} {
		req, _ := http.NewRequest(http.MethodGet, endpoint+path, nil)
		req.Header.Set("X-API-Key", "primary")
		resp := serveDirect(mux, req)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown path %s status = %d, want 404", path, resp.StatusCode)
		}
	}

	results := make(map[string]map[string]any)
	sessions := make(map[string]string)
	for _, test := range []struct {
		path string
		want []string
	}{
		{path: "/mcp", want: []string{"get_event", "search_events"}},
		{path: "/mcp/compat", want: []string{"get_event_v2", "search_events_v2"}},
	} {
		got, sessionID := listHTTPTools(t, mux, endpoint+test.path, "primary")
		sessions[test.path] = sessionID
		for _, name := range test.want {
			if _, ok := got[name]; !ok {
				t.Errorf("%s does not list %q", test.path, name)
			}
		}
		callResult := callHTTPTool(t, mux, endpoint+test.path, "primary", sessionID, "list_calendars", map[string]any{})
		results[test.path] = callResult
		if callResult["isError"] == true {
			t.Errorf("%s list_calendars returned error: %#v", test.path, callResult)
		}
	}
	modernResult, _ := json.Marshal(results["/mcp"])
	compatResult, _ := json.Marshal(results["/mcp/compat"])
	if string(modernResult) != string(compatResult) {
		t.Fatalf("shared list_calendars output differs: modern=%s compat=%s", modernResult, compatResult)
	}
	modernError := callHTTPTool(t, mux, endpoint+"/mcp/modern", "primary", sessions["/mcp"], "get_event", map[string]any{"calendar_id": "unknown:calendar", "event_id": "event"})
	compatError := callHTTPTool(t, mux, endpoint+"/mcp/compat", "primary", sessions["/mcp/compat"], "get_event_v2", map[string]any{"calendar_id": "unknown:calendar", "event_id": "event"})
	modernErrorJSON, _ := json.Marshal(modernError)
	compatErrorJSON, _ := json.Marshal(compatError)
	if string(modernErrorJSON) != string(compatErrorJSON) {
		t.Fatalf("renamed get_event error differs: modern=%s compat=%s", modernErrorJSON, compatErrorJSON)
	}

	// A session initialized on one catalog cannot change the tool contract on another.
	foreign := listHTTPToolsForSession(t, mux, endpoint+"/mcp/modern", "primary", sessions["/mcp/compat"])
	if _, ok := foreign["search_events"]; !ok {
		t.Fatal("compat session exposed the wrong catalog on /mcp/modern")
	}
	if _, ok := foreign["search_events_v2"]; ok {
		t.Fatal("compat session exposed compat tool names on /mcp/modern")
	}

	// Requests to the two catalog paths can run concurrently without mixing tool sets.
	var wg sync.WaitGroup
	for _, test := range []struct {
		path    string
		session string
		want    string
	}{
		{path: "/mcp/modern", session: sessions["/mcp"], want: "search_events"},
		{path: "/mcp/compat", session: sessions["/mcp/compat"], want: "search_events_v2"},
	} {
		test := test
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools := listHTTPToolsForSession(t, mux, endpoint+test.path, "primary", test.session)
			if _, ok := tools[test.want]; !ok {
				t.Errorf("concurrent %s list omitted %s", test.path, test.want)
			}
		}()
	}
	wg.Wait()

	// No redirects for mutating requests to slashless aliases.
	requestBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"update_event","arguments":{}}}`
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/mcp/modern", strings.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer primary")
	req.Header.Set("Content-Type", "application/json")
	resp := serveDirect(mux, req)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		t.Fatalf("POST /mcp/modern redirected with status %d", resp.StatusCode)
	}
}

func listHTTPTools(t *testing.T, mux *http.ServeMux, endpoint, token string) (map[string]struct{}, string) {
	t.Helper()
	initialize := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"catalog-test","version":"1"}}}`, mcp.LATEST_PROTOCOL_VERSION)
	resp := postMCP(t, mux, endpoint+"/", token, "", initialize)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response omitted session ID")
	}
	return listHTTPToolsForSession(t, mux, endpoint, token, sessionID), sessionID
}

func listHTTPToolsForSession(t *testing.T, mux *http.ServeMux, endpoint, token, sessionID string) map[string]struct{} {
	t.Helper()
	resp := postMCP(t, mux, endpoint, token, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status = %d", resp.StatusCode)
	}
	var envelope struct {
		Result struct {
			Tools []mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	result := make(map[string]struct{}, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		result[tool.Name] = struct{}{}
	}
	return result
}

func callHTTPTool(t *testing.T, mux *http.ServeMux, endpoint, token, sessionID, name string, args map[string]any) map[string]any {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":%s}`, params)
	resp := postMCP(t, mux, endpoint+"/", token, sessionID, request)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call status = %d", resp.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		t.Fatalf("tools/call returned JSON-RPC error: %s", envelope.Error)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		t.Fatalf("tools/call omitted result: %s", envelope.Result)
	}
	var result map[string]any
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	return result
}

func postMCP(t *testing.T, mux *http.ServeMux, endpoint, token, sessionID, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	return serveDirect(mux, req)
}

func TestRegisterWithOptionsValidationAndDisabledModern(t *testing.T) {
	reg := calendar.NewRegistry(nil)
	app := application.New(reg)
	for _, opts := range []Options{{CatalogMode: "bad"}, {CatalogMode: CatalogModern}} {
		mux := http.NewServeMux()
		if err := RegisterWithOptions(mux, reg, app, auth.Options{AllowUnauthenticated: true}, opts); err == nil {
			t.Errorf("RegisterWithOptions(%+v) unexpectedly succeeded", opts)
		}
	}
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, reg, app, auth.Options{APIKey: "key"}, Options{CatalogMode: CatalogCompat}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://catalog.test/mcp/modern", nil)
	req.Header.Set("X-API-Key", "key")
	resp := serveDirect(mux, req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled modern endpoint status = %d, want 404", resp.StatusCode)
	}
}

func TestCatalogBodyLimit(t *testing.T) {
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, calendar.NewRegistry(nil), application.New(calendar.NewRegistry(nil)), auth.Options{AllowUnauthenticated: true}, Options{EnableV2: true}); err != nil {
		t.Fatal(err)
	}
	body := `{"padding":"` + strings.Repeat("x", 1<<20+1) + `"}`
	for _, path := range []string{"/mcp", "/mcp/compat", "/mcp/modern"} {
		req, _ := http.NewRequest(http.MethodPost, "http://catalog.test"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := serveDirect(mux, req)
		message, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(message), "request body too large") {
			t.Fatalf("oversized body for %s status=%d body=%q", path, resp.StatusCode, message)
		}
	}
}

func serveDirect(handler http.Handler, request *http.Request) *http.Response {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Result()
}
