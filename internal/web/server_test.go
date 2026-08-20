package web

import (
	"bytes"
	"context"
	"encoding/base64"
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
	server, err := New(store, connections.New(store, cipher), nil, func(context.Context) ([]calendar.Provider, error) { return nil, nil }, Config{PublicURL: "https://calendar.example", TrustForwardAuth: trustForwardAuth, AllowUnauthenticated: allowUnauthenticated, AppleCalDAVURL: "https://caldav.icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

func TestPagesRenderWithoutFabricatedOperationalData(t *testing.T) {
	handler := newTestHandler(t, false, true)
	for _, path := range []string{"/", "/connections", "/rules", "/rules/new", "/runs", "/settings"} {
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
	for _, path := range []string{"templates/app.html", "assets/app.css", "assets/htmx.min.js"} {
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
