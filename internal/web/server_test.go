package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

func newTestHandler(t *testing.T, trustForwardAuth, allowUnauthenticated bool) http.Handler {
	return newTestHandlerWithConfig(t, Config{PublicURL: "https://calendar.example", TrustForwardAuth: trustForwardAuth, AllowUnauthenticated: allowUnauthenticated, AppleCalDAVURL: "https://caldav.icloud.com"})
}

func newTestHandlerWithConfig(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	server, err := New(store, connections.New(store, cipher), nil, func(context.Context) ([]calendar.Provider, error) { return nil, nil }, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

func TestSettingsSPAAndBootstrapExposeStatusWithoutRenderingKeys(t *testing.T) {
	handler := newTestHandlerWithConfig(t, Config{
		PublicURL:              "https://calendar.example",
		AllowUnauthenticated:   true,
		MCPAPIKey:              "primary-secret",
		LegacyAPIKeyConfigured: true,
		AppleCalDAVURL:         "https://caldav.icloud.com",
	})
	req := httptest.NewRequest(http.MethodGet, "https://calendar.example/settings", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, secret := range []string{"primary-secret", "legacy-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("Settings HTML contains secret %q", secret)
		}
	}
	if !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatalf("Settings does not serve the React shell: %s", body)
	}

	bootstrapReq := httptest.NewRequest(http.MethodGet, "https://calendar.example/api/ui/control-plane", nil)
	bootstrapRes := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapRes, bootstrapReq)
	if bootstrapRes.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, body = %s", bootstrapRes.Code, bootstrapRes.Body.String())
	}
	for _, secret := range []string{"primary-secret", "legacy-secret"} {
		if strings.Contains(bootstrapRes.Body.String(), secret) {
			t.Fatalf("Bootstrap contains secret %q", secret)
		}
	}
	for _, expected := range []string{`"mcp_endpoint":"https://calendar.example/mcp"`, `"legacy_api_key_configured":true`} {
		if !strings.Contains(bootstrapRes.Body.String(), expected) {
			t.Fatalf("Bootstrap does not contain %q: %s", expected, bootstrapRes.Body.String())
		}
	}
}

func TestMCPKeyRevealRequiresForwardAuthOriginAndCSRF(t *testing.T) {
	handler := newTestHandlerWithConfig(t, Config{
		PublicURL:        "https://calendar.example",
		TrustForwardAuth: true,
		MCPAPIKey:        "primary-secret",
		AppleCalDAVURL:   "https://caldav.icloud.com",
	})

	tests := []struct {
		name       string
		identity   string
		origin     string
		csrfCookie string
		csrfForm   string
		wantStatus int
	}{
		{name: "missing Authentik identity", origin: "https://calendar.example", csrfCookie: "token", csrfForm: "token", wantStatus: http.StatusUnauthorized},
		{name: "different origin", identity: "admin", origin: "https://evil.example", csrfCookie: "token", csrfForm: "token", wantStatus: http.StatusForbidden},
		{name: "invalid CSRF", identity: "admin", origin: "https://calendar.example", csrfCookie: "right", csrfForm: "wrong", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			form := url.Values{"csrf_token": {test.csrfForm}}
			req := httptest.NewRequest(http.MethodPost, "https://calendar.example/settings/mcp-key/reveal", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", test.origin)
			if test.identity != "" {
				req.Header.Set("X-authentik-username", test.identity)
			}
			req.AddCookie(&http.Cookie{Name: "calendar_csrf", Value: test.csrfCookie})
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, test.wantStatus)
			}
			assertNoStoreHeaders(t, res)
			if strings.Contains(res.Body.String(), "primary-secret") {
				t.Fatal("error response contains primary key")
			}
		})
	}
}

func TestMCPKeyRevealReturnsPrimaryOnly(t *testing.T) {
	handler := newTestHandlerWithConfig(t, Config{
		PublicURL:              "https://calendar.example",
		TrustForwardAuth:       true,
		MCPAPIKey:              "primary-secret",
		LegacyAPIKeyConfigured: true,
		AppleCalDAVURL:         "https://caldav.icloud.com",
	})
	res := revealMCPKey(t, handler)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	assertNoStoreHeaders(t, res)
	var response map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 1 || response["token"] != "primary-secret" {
		t.Fatalf("response = %#v", response)
	}
	if strings.Contains(res.Body.String(), "legacy-secret") {
		t.Fatal("response contains legacy key")
	}
}

func TestMCPKeyRevealUnavailableIsSafeAndNotCacheable(t *testing.T) {
	handler := newTestHandlerWithConfig(t, Config{
		PublicURL:        "https://calendar.example",
		TrustForwardAuth: true,
		AppleCalDAVURL:   "https://caldav.icloud.com",
	})
	res := revealMCPKey(t, handler)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	assertNoStoreHeaders(t, res)
	if strings.Contains(res.Body.String(), "token") {
		t.Fatalf("safe error contains token field: %s", res.Body.String())
	}
}

func revealMCPKey(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"csrf_token": {"token"}}
	req := httptest.NewRequest(http.MethodPost, "https://calendar.example/settings/mcp-key/reveal", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://calendar.example")
	req.Header.Set("X-authentik-username", "admin")
	req.AddCookie(&http.Cookie{Name: "calendar_csrf", Value: "token"})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func assertNoStoreHeaders(t *testing.T, res *httptest.ResponseRecorder) {
	t.Helper()
	if got := res.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := res.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
	}
}

func TestPagesRenderWithoutFabricatedOperationalData(t *testing.T) {
	handler := newTestHandler(t, false, true)
	for _, path := range []string{"/app", "/connections", "/rules", "/rules/new", "/runs", "/settings"} {
		req := httptest.NewRequest(http.MethodGet, "https://calendar.example"+path, nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, res.Code, res.Body.String())
		}
		if strings.Contains(res.Body.String(), "Interactive design mockup") {
			t.Fatalf("GET %s contains mockup data", path)
		}
	}
}

func TestPublicPagesBypassForwardAuthWhileApplicationStaysProtected(t *testing.T) {
	handler := newTestHandler(t, true, false)
	for _, test := range []struct {
		path, text string
	}{
		{"/", "Why Calendar platform requests Google Calendar data"},
		{"/privacy", "Google API Services User Data Policy"},
		{"/terms", "Calendar changes and synchronization"},
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://calendar.example"+test.path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", test.path, res.Code, res.Body.String())
		}
		if !strings.Contains(res.Body.String(), test.text) {
			t.Fatalf("GET %s does not contain %q", test.path, test.text)
		}
		if test.path == "/" {
			for _, required := range []string{
				"<title>Calendar platform</title>",
				"<h1>Calendar platform</h1>",
				"The purpose of Calendar platform is to let one person connect",
				"list available calendars",
				"Google Calendar data is not sold or used for advertising",
			} {
				if !strings.Contains(res.Body.String(), required) {
					t.Fatalf("homepage does not contain required identity/purpose text %q", required)
				}
			}
		}
		if res.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("GET %s has no Content-Security-Policy", test.path)
		}
	}

	for _, path := range []string{"/app", "/connections", "/rules", "/runs", "/settings"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://calendar.example"+path, nil))
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s status = %d, want %d", path, res.Code, http.StatusUnauthorized)
		}
	}

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://calendar.example/not-public", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func TestSPAAssetsBypassForwardAuth(t *testing.T) {
	handler := newTestHandler(t, true, false)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://calendar.example/spa/placeholder.txt", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("unauthenticated SPA asset status = %d, want %d", res.Code, http.StatusOK)
	}
}

func TestMarkdownRenderingOmitsUnsafeHTMLAndLinks(t *testing.T) {
	rendered, err := renderMarkdown([]byte("<script>alert('x')</script>\n\n[unsafe](javascript:alert(1))"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "<script") || strings.Contains(text, "javascript:") {
		t.Fatalf("unsafe Markdown rendered as %q", text)
	}
}

func TestForwardAuthAndCSRFProtection(t *testing.T) {
	handler := newTestHandler(t, true, false)
	unauthenticated := httptest.NewRequest(http.MethodGet, "https://calendar.example/connections", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, unauthenticated)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", res.Code)
	}

	form := url.Values{"username": {"user@example.com"}, "app_password": {"secret"}, "csrf_token": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "https://calendar.example/connections/apple", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://calendar.example")
	req.Header.Set("X-authentik-username", "admin")
	req.AddCookie(&http.Cookie{Name: "calendar_csrf", Value: "right"})
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("invalid CSRF status = %d", res.Code)
	}
}

func TestUIFailsClosedWithoutForwardAuthOrExplicitBypass(t *testing.T) {
	handler := newTestHandler(t, false, false)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://calendar.example/connections", nil))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
	}
}

func TestCreateRuleRejectsMalformedNumericFields(t *testing.T) {
	handler := newTestHandler(t, false, true)
	form := url.Values{"interval_seconds": {"not-a-number"}, "lookback_days": {"0"}, "lookahead_days": {"14"}, "csrf_token": {"token"}}
	req := httptest.NewRequest(http.MethodPost, "https://calendar.example/rules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://calendar.example")
	req.AddCookie(&http.Cookie{Name: "calendar_csrf", Value: "token"})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/rules/new?status=invalid_rule" {
		t.Fatalf("status=%d location=%q", res.Code, res.Header().Get("Location"))
	}
}

func TestEmbeddedUIHasNoRuntimeCDN(t *testing.T) {
	for _, path := range []string{"templates/app.html", "assets/app.css", "assets/app.js"} {
		data, err := fs.ReadFile(content, path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, "https://") || strings.Contains(text, "http://") {
			t.Fatalf("%s contains runtime remote URL", path)
		}
	}
}

func TestAuthenticatedAppUsesTopLevelNavigation(t *testing.T) {
	data, err := fs.ReadFile(content, "templates/app.html")
	if err != nil {
		t.Fatal(err)
	}
	templateText := string(data)
	if strings.Contains(templateText, "hx-boost") {
		t.Fatal("authenticated app must not use HTMX-boosted navigation behind forward auth")
	}
	if strings.Contains(templateText, "htmx.min.js") {
		t.Fatal("authenticated app must not load the unused HTMX runtime")
	}
	if !strings.Contains(templateText, `<script defer src="/assets/app.js"></script>`) {
		t.Fatal("authenticated app must load the same-origin action progress script")
	}
}

func TestActionControlsExposePendingState(t *testing.T) {
	data, err := fs.ReadFile(content, "templates/app.html")
	if err != nil {
		t.Fatal(err)
	}
	templateText := string(data)
	for _, label := range []string{"Connecting…", "Verifying…", "Deleting…", "Saving…", "Starting…", "Updating…"} {
		marker := `data-pending-label="` + label + `"`
		if !strings.Contains(templateText, marker) {
			t.Fatalf("authenticated app is missing pending state %q", label)
		}
	}
}
