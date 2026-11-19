//go:build calendar_mcp_legacy_fixture

package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/cachedpages"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func cachedHTTPFixture(t *testing.T) (*storage.Store, *cachedpages.Service, cachedpages.Request) {
	t.Helper()
	store, err := storage.Open(t.Context(), "sqlite://"+filepath.Join(t.TempDir(), "cached-sdk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const cid = "google:sdk:primary"
	if err = store.CreateConnection(t.Context(), storage.Connection{ID: "sdk", Provider: "google", DisplayName: "Synthetic SDK", Status: "connected", EncryptedCredentials: []byte("synthetic-no-provider"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	snap, err := store.BeginAuthorization(t.Context(), "sdk", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteGoogleAuthorization(t.Context(), snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: cid, ConnectionID: "sdk", ProviderCalendarID: "primary", Name: "Synthetic primary"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	window := storage.SyncWindow{Start: now.Add(-24 * time.Hour), End: now.Add(24 * time.Hour)}
	if err = store.EnsureCalendarSyncStates(t.Context(), now, window); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.CaptureWriterTicket(t.Context(), cid)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		event := calendar.EventV2{ID: fmt.Sprintf("event-%d", i), CalendarID: cid, Provider: "google", Title: fmt.Sprintf("Useful title %d", i), Description: strings.Repeat("PRIVATE DESCRIPTION ", 3000), Start: calendar.EventTime{DateTime: now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
		if err = store.UpsertCachedEventWithTicket(t.Context(), ticket, event, now); err != nil {
			t.Fatal(err)
		}
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{71}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := cachedpages.NewService(store, cipher, cachedpages.Options{Window: window, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return store, reader, cachedpages.Request{CalendarIDs: []string{cid}, Start: now.Add(-time.Hour), End: now.Add(time.Hour), PageSize: 1}
}

func newCachedSDKClient(t *testing.T, endpoint, key string) *client.Client {
	t.Helper()
	c, err := client.NewStreamableHttpClient(endpoint, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + key}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := mcp.InitializeRequest{}
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "synthetic-cached-sdk", Version: "1"}
	if _, err = c.Initialize(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return c
}

func cachedSDKCall(t *testing.T, c *client.Client, request cachedpages.Request, token string) *mcp.CallToolResult {
	t.Helper()
	call := mcp.CallToolRequest{}
	call.Params.Name = "get_cached_events"
	call.Params.Arguments = map[string]any{"calendar_ids": request.CalendarIDs, "start": request.Start.Format(time.RFC3339), "end": request.End.Format(time.RFC3339), "page_size": request.PageSize, "page_token": token}
	result, err := c.CallTool(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func decodeCachedSDKPage(t *testing.T, result *mcp.CallToolResult) cachedpages.Page {
	t.Helper()
	if result.IsError {
		t.Fatalf("cached call failed: %+v", result.Content)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > cachedpages.MaxResultBytes || bytes.Contains(wire, []byte("PRIVATE DESCRIPTION")) {
		t.Fatal("unbounded or non-summary SDK result")
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var page cachedpages.Page
	if err = json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestCachedEventsActualSDKCallerSessionAndFreshRights(t *testing.T) {
	store, reader, request := cachedHTTPFixture(t)
	reg := calendar.NewRegistry(nil)
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "primary", LegacyAPIKey: "legacy"}, Options{EnableV2: true, EventReadModelEnabled: true, CatalogMode: CatalogModern, CachedEvents: reader}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	firstClient := newCachedSDKClient(t, srv.URL+"/mcp/modern", "primary")
	listed, err := firstClient.ListTools(t.Context(), mcp.ListToolsRequest{})
	if err != nil || len(listed.Tools) != 13 {
		t.Fatalf("modern catalog=%+v error=%v", listed, err)
	}
	first := decodeCachedSDKPage(t, cachedSDKCall(t, firstClient, request, ""))
	if len(first.Items) != 1 || first.Items[0].Title != "Useful title 0" || first.NextPageToken == "" || first.Consistency != "cached_weak" {
		t.Fatalf("first page=%+v", first)
	}
	second := decodeCachedSDKPage(t, cachedSDKCall(t, firstClient, request, first.NextPageToken))
	if len(second.Items) != 1 || second.Items[0].EventID != "event-1" {
		t.Fatalf("same session continuation=%+v", second)
	}
	otherSession := newCachedSDKClient(t, srv.URL+"/mcp/modern/", "primary")
	page := decodeCachedSDKPage(t, cachedSDKCall(t, otherSession, request, first.NextPageToken))
	if page.Items[0].EventID != "event-1" {
		t.Fatal("same accepted key could not continue in another session")
	}
	legacy := newCachedSDKClient(t, srv.URL+"/mcp/modern", "legacy")
	if got := cachedSDKCall(t, legacy, request, first.NextPageToken); !got.IsError {
		t.Fatal("cursor transferred between accepted credentials")
	}
	// An established session cannot replace current HTTP authorization.
	for _, credential := range []string{"", "wrong"} {
		body := `{"jsonrpc":"2.0","id":99,"method":"tools/list"}`
		r, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mcp/modern", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Mcp-Session-Id", firstClient.GetSessionId())
		r.Header.Set("X-User-ID", "primary")
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		response, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("session bypass status=%d", response.StatusCode)
		}
	}
	if _, err = store.BeginAuthorization(t.Context(), "sdk", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := cachedSDKCall(t, firstClient, request, first.NextPageToken); !got.IsError {
		t.Fatal("closed receipt continuation leaked cached payload")
	}
	if got := cachedSDKCall(t, firstClient, request, ""); !got.IsError {
		t.Fatal("closed receipt root query returned a false empty page")
	}
}

func TestCachedEventsActualSDKRegistrationPrerequisites(t *testing.T) {
	_, reader, _ := cachedHTTPFixture(t)
	for _, tc := range []struct {
		name string
		opts Options
		auth auth.Options
		path string
		want int
	}{
		{"modern", Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: reader}, auth.Options{APIKey: "primary"}, "/mcp/modern", 13},
		{"compat", Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: reader}, auth.Options{APIKey: "primary"}, "/mcp/compat", 16},
		{"default_compat", Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: reader}, auth.Options{APIKey: "primary"}, "/mcp", 16},
		{"default_modern", Options{EnableV2: true, EventReadModelEnabled: true, CatalogMode: CatalogModern, CachedEvents: reader}, auth.Options{APIKey: "primary"}, "/mcp", 12},
		{"no_read_model", Options{EnableV2: true, CachedEvents: reader}, auth.Options{APIKey: "primary"}, "/mcp/modern", 12},
		{"no_dependency", Options{EnableV2: true, EventReadModelEnabled: true}, auth.Options{APIKey: "primary"}, "/mcp/modern", 12},
		{"uninitialized_dependency", Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: &cachedpages.Service{}}, auth.Options{APIKey: "primary"}, "/mcp/modern", 12},
		{"unauthenticated", Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: reader}, auth.Options{AllowUnauthenticated: true}, "/mcp/modern", 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := calendar.NewRegistry(nil)
			mux := http.NewServeMux()
			if err := RegisterWithOptions(mux, reg, application.New(reg), tc.auth, tc.opts); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := newCachedSDKClient(t, srv.URL+tc.path, "primary")
			listed, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
			if err != nil || len(listed.Tools) != tc.want {
				t.Fatalf("tools=%+v error=%v", listed, err)
			}
		})
	}
}

func TestCachedEventsActualHTTPStrictArgumentsAndBodyLimit(t *testing.T) {
	_, reader, _ := cachedHTTPFixture(t)
	reg := calendar.NewRegistry(nil)
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "primary"}, Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: reader}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newCachedSDKClient(t, srv.URL+"/mcp/modern", "primary")
	base := `"calendar_ids":["google:sdk:primary"],"start":"2026-11-17T11:00:00Z","end":"2026-11-17T13:00:00Z"`
	for _, args := range []string{`{` + base + `,"page_size":1,"page_size":2}`, `{` + base + `,"caller":"primary"}`, `{` + base + `,"calendar_ids":["google:sdk:primary","google:sdk:primary"]}`} {
		body := `{"jsonrpc":"2.0","id":123,"method":"tools/call","params":{"name":"get_cached_events","arguments":` + args + `}}`
		data := cachedRawHTTP(t, srv, c.GetSessionId(), body)
		if !bytes.Contains(data, []byte("invalid_argument")) || bytes.Contains(data, []byte("Useful title")) {
			t.Fatalf("strict arguments response: %.300s", data)
		}
	}
	oversized := `{"jsonrpc":"2.0","id":123,"method":"tools/call","params":{"name":"get_cached_events","arguments":{"oversized":"` + strings.Repeat("x", 1<<20) + `"}}}`
	if data := cachedRawHTTP(t, srv, c.GetSessionId(), oversized); bytes.Contains(data, []byte("Useful title")) {
		t.Fatal("oversized request reached cached payload")
	}
}

func cachedRawHTTP(t *testing.T, srv *httptest.Server, session, body string) []byte {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/mcp/modern", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Authorization", "Bearer primary")
	r.Header.Set("Mcp-Session-Id", session)
	response, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type pausedCachedWriter struct {
	http.ResponseWriter
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *pausedCachedWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseWriter.Write(data)
}
func (w *pausedCachedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func TestCachedEventsSlowHTTPWriterAndCancellationReleaseStore(t *testing.T) {
	store, reader, _ := cachedHTTPFixture(t)
	reg := calendar.NewRegistry(nil)
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "primary"}, Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: reader}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slow") == "1" {
			w = &pausedCachedWriter{ResponseWriter: w, entered: entered, release: release}
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := newCachedSDKClient(t, srv.URL+"/mcp/modern", "primary")
	if c.GetSessionId() == "" {
		t.Fatal("SDK did not establish a session")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"get_cached_events","arguments":{"calendar_ids":["google:sdk:primary"],"start":"2026-11-17T11:00:00Z","end":"2026-11-17T13:00:00Z","page_size":1}}}`
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mcp/modern?slow=1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer primary")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Mcp-Session-Id", c.GetSessionId())
	done := make(chan error, 1)
	go func() {
		response, err := srv.Client().Do(r)
		if response != nil {
			_, readErr := io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if err == nil {
				err = readErr
			}
		}
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		close(release)
		t.Fatalf("HTTP output not reached: %v", err)
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	// SQLite's unchanged pool has exactly one slot. A read transaction or
	// connection retained by the response would block this actual Store call.
	checkCtx, stop := context.WithTimeout(t.Context(), time.Second)
	_, checkErr := store.ConnectionByID(checkCtx, "sdk")
	stop()
	cancel()
	close(release)
	if checkErr != nil {
		t.Fatalf("slow HTTP writer retained Store lease: %v", checkErr)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled HTTP call did not end")
	}
}

func TestCachedEventsActualSDKRevocationAfterPackBeforeFinalValidation(t *testing.T) {
	store, _, request := cachedHTTPFixture(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{71}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var clockCalls atomic.Int32
	reader, err := cachedpages.NewService(store, cipher, cachedpages.Options{Window: storage.SyncWindow{Start: now.Add(-24 * time.Hour), End: now.Add(24 * time.Hour)}, Now: func() time.Time {
		// Initial deadline, cursor Encode, then the post-Pack expiry check.
		if clockCalls.Add(1) == 3 {
			close(entered)
			<-release
		}
		return now
	}})
	if err != nil {
		t.Fatal(err)
	}
	reg := calendar.NewRegistry(nil)
	mux := http.NewServeMux()
	if err = RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "primary"}, Options{EnableV2: true, EventReadModelEnabled: true, CachedEvents: reader}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newCachedSDKClient(t, srv.URL+"/mcp/modern", "primary")
	call := mcp.CallToolRequest{}
	call.Params.Name = "get_cached_events"
	call.Params.Arguments = map[string]any{"calendar_ids": request.CalendarIDs, "start": request.Start.Format(time.RFC3339), "end": request.End.Format(time.RFC3339), "page_size": 1}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() { r, e := c.CallTool(ctx, call); done <- outcome{r, e} }()
	select {
	case <-entered:
	case got := <-done:
		close(release)
		t.Fatalf("post-Pack barrier not reached: %+v", got)
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	_, revokeErr := store.BeginAuthorization(ctx, "sdk", nil, now.Add(time.Second))
	close(release)
	if revokeErr != nil {
		t.Fatal(revokeErr)
	}
	got := <-done
	if got.err != nil || got.result == nil || !got.result.IsError {
		t.Fatalf("revoked SDK result=%+v error=%v", got.result, got.err)
	}
	wire, err := json.Marshal(got.result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("Useful title")) || len(wire) > cachedpages.MaxResultBytes {
		t.Fatal("packed title escaped fresh validation")
	}
}

func TestMainMCPPublishesAndServesCachedGetEvents(t *testing.T) {
	store, reader, query := cachedHTTPFixture(t)
	now := query.Start.Add(time.Hour)
	state, err := store.ClaimDueCalendarSync(t.Context(), "fixture", now, now.Add(time.Hour))
	if err != nil || state == nil {
		t.Fatalf("claim synthetic calendar: %v", err)
	}
	if err := store.ApplyEventSyncPage(t.Context(), *state, storage.EventSyncBatch{NextCursor: "fixture"}, true, now); err != nil {
		t.Fatal(err)
	}
	reg := calendar.NewRegistry(nil)
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "primary"},
		Options{EnableV2: true, EventReadModelEnabled: true, CatalogMode: CatalogModern, CachedEvents: reader}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newCachedSDKClient(t, srv.URL+"/mcp", "primary")
	listed, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "get_cached_events" {
			t.Fatal("legacy cached alias exposed on the main endpoint")
		}
		if tool.Name == "get_events" {
			properties := tool.InputSchema.Properties
			found = properties["calendar_ids"] != nil && properties["page_size"] != nil && properties["calendar_id"] == nil
		}
	}
	if !found {
		t.Fatal("cached get_events schema missing on main endpoint")
	}
	call := mcp.CallToolRequest{}
	call.Params.Name = "get_events"
	call.Params.Arguments = map[string]any{
		"start": query.Start.Format(time.RFC3339), "end": query.End.Format(time.RFC3339), "page_size": 1,
	}
	result, err := c.CallTool(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	page := decodeCachedSDKPage(t, result)
	if len(page.Items) != 1 || page.Items[0].Title != "Useful title 0" || page.NextPageToken == "" || page.Consistency != "cached_weak" {
		t.Fatalf("unexpected main endpoint page: %+v", page)
	}
}
