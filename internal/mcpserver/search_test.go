package mcpserver

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
)

type listedApple struct{ *apple.Provider }

func (p listedApple) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	return []calendar.Calendar{{ID: "/cal/"}}, nil
}

type mcpSearchProvider struct {
	calendar.Provider
	name string
}

func (p mcpSearchProvider) Name() string { return p.name }
func (p mcpSearchProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	return []calendar.Calendar{{ID: "primary"}}, nil
}
func (p mcpSearchProvider) SearchEventsV2(context.Context, calendar.SearchEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "google-event", Title: "JetBrains", Start: calendar.EventTime{Date: "2026-10-02"}}}, Complete: true}, nil
}

func TestMCPSearchIncludesBoundedAppleAndClassifiesUnbounded(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "REPORT" {
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "unexpected", 400)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		_, _ = fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
		for i := 1; i <= 2; i++ {
			ics := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:apple-%d\r\nSUMMARY:JetBrains\r\nDTSTART;VALUE=DATE:2026100%d\r\nDTEND;VALUE=DATE:2026100%d\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", i, i, i+1)
			var escaped strings.Builder
			_ = xml.EscapeText(&escaped, []byte(ics))
			_, _ = fmt.Fprintf(w, `<D:response><D:href>/cal/%d.ics</D:href><D:propstat><D:prop><C:calendar-data>%s</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, i, escaped.String())
		}
		_, _ = fmt.Fprint(w, `</D:multistatus>`)
	}))
	defer fake.Close()
	ap, err := apple.New("fixture", "fixture", fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	reg := calendar.NewRegistry([]calendar.Provider{listedApple{ap}, mcpSearchProvider{name: "google"}})
	c, err := client.NewInProcessClient(buildServer(reg, application.New(reg), true))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "search-test", Version: "1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatal(err)
	}
	request := mcp.CallToolRequest{}
	request.Params.Name = "search_events_v2"
	request.Params.Arguments = map[string]any{"query": "JetBrains", "start": "2026-10-01T00:00:00+02:00", "end": "2026-11-01T00:00:00+01:00", "max_results": 1}
	result, err := c.CallTool(ctx, request)
	if err != nil || result.IsError {
		t.Fatalf("result %#v error %v", result, err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var page calendar.Page[calendar.EventV2]
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if !page.Complete || len(page.Items) != 3 || len(page.Sources) != 2 {
		t.Fatalf("page %s", raw)
	}
	for _, item := range page.Items {
		if item.Provider == "apple" && !strings.HasPrefix(item.ID, "apple:") {
			t.Fatalf("unrouted Apple event %#v", item)
		}
	}
	request.Params.Arguments = map[string]any{"query": "JetBrains"}
	result, err = c.CallTool(ctx, request)
	if err != nil || result.IsError {
		t.Fatalf("%#v %v", result, err)
	}
	raw, _ = json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if page.Complete {
		t.Fatalf("unbounded Apple marked complete: %s", raw)
	}
	found := false
	for _, source := range page.Sources {
		if source.Provider == "apple" {
			found = true
			if source.Error == nil || source.Error.Code != calendar.ErrorUnsupportedCapability || source.Error.Retryable {
				t.Fatalf("source %#v", source)
			}
		}
	}
	if !found {
		t.Fatal("Apple source missing")
	}
}
