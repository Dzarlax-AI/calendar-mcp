package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
)

type deniedWriteFence struct{}

func (deniedWriteFence) BeginWriteDispatch(context.Context, []string) (func(context.Context) error, error) {
	return nil, errors.New("synthetic busy")
}

type fencedCatalogProvider struct {
	*catalogBehaviorProvider
	legacyDeletes int
}

func (p *fencedCatalogProvider) DeleteEvent(context.Context, string, string) error {
	p.legacyDeletes++
	return nil
}

func TestMCPModernAndLegacyWritesHonorFence(t *testing.T) {
	p := &fencedCatalogProvider{catalogBehaviorProvider: &catalogBehaviorProvider{name: "google", capabilities: calendar.CalendarCapabilities{MutationScopes: []calendar.MutationScope{calendar.ScopeSingle}, NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}, created: &calendar.EventV2{ID: "synthetic"}}}
	reg := calendar.NewRegistry([]calendar.Provider{p}, calendar.RegistryOptions{WriteGuard: deniedWriteFence{}})
	mux := http.NewServeMux()
	if err := RegisterWithOptions(mux, reg, application.New(reg), auth.Options{APIKey: "key"}, Options{EnableV2: true, CatalogMode: CatalogModern}); err != nil {
		t.Fatal(err)
	}
	endpoint := "http://catalog.test"
	_, modern := listHTTPTools(t, mux, endpoint+"/mcp/modern", "key")
	_, compat := listHTTPTools(t, mux, endpoint+"/mcp/compat", "key")
	result := callHTTPTool(t, mux, endpoint+"/mcp/modern", "key", modern, "create_event", map[string]any{"calendar_id": "google:primary", "event": map[string]any{"title": "synthetic", "start": map[string]any{"date": "2026-10-26"}, "end": map[string]any{"date": "2026-10-27"}}})
	if result["isError"] != true || len(p.createRequests) != 0 {
		t.Fatalf("modern fence bypass: %#v", result)
	}
	result = callHTTPTool(t, mux, endpoint+"/mcp/compat", "key", compat, "delete_event", map[string]any{"calendar_id": "google:primary", "event_id": "event-1"})
	if result["isError"] != true || p.legacyDeletes != 0 {
		t.Fatalf("legacy fence bypass: %#v", result)
	}
}
