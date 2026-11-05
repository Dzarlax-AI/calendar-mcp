package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func markerTestProvider(t *testing.T, fn func(*http.Request) string) *Provider {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(fn(req))), Request: req}, nil
	})}
	svc, err := gcal.NewService(t.Context(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	return &Provider{svc: svc}
}
func TestMarkerLookupFiltersORResultsAndTraversesPages(t *testing.T) {
	calls := 0
	p := markerTestProvider(t, func(r *http.Request) string {
		calls++
		if r.Method != "GET" || r.URL.Query().Get("timeMin") != "" || r.URL.Query().Get("timeMax") != "" {
			t.Errorf("unexpected request: %v", r.URL)
		}
		props := r.URL.Query()["privateExtendedProperty"]
		if len(props) != 1 || props[0] != "calendar_source_event=source" {
			t.Errorf("unexpected search properties: %v", props)
		}
		if calls == 1 {
			return `{"items":[{"id":"wrong","extendedProperties":{"private":{"calendar_sync_rule":"another","calendar_source_event":"source"}}}],"nextPageToken":"next"}`
		}
		if r.URL.Query().Get("pageToken") != "next" {
			t.Error("missing next token")
		}
		return `{"items":[{"id":"right","extendedProperties":{"private":{"calendar_sync_rule":"rule","calendar_source_event":"source"}}}]}`
	})
	e, err := p.FindEventBySyncMarkerV2(context.Background(), "calendar", "rule", "source")
	if err != nil || e == nil || e.ID != "right" || calls != 2 {
		t.Fatalf("lookup: %+v %v calls=%d", e, err, calls)
	}
}
func TestMarkerLookupFailsClosedOnPaginationAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(int) string
	}{
		{"repeated token", func(int) string { return `{"nextPageToken":"repeat"}` }},
		{"page limit", func(i int) string { return fmt.Sprintf(`{"nextPageToken":"page-%d"}`, i) }},
		{"missing properties", func(int) string { return `{"items":[{"id":"unknown"}]}` }},
		{"duplicate match", func(int) string {
			return `{"items":[{"id":"one","extendedProperties":{"private":{"calendar_sync_rule":"rule","calendar_source_event":"source"}}},{"id":"two","extendedProperties":{"private":{"calendar_sync_rule":"rule","calendar_source_event":"source"}}}]}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := markerTestProvider(t, func(*http.Request) string { calls++; return tc.fn(calls) })
			event, err := p.FindEventBySyncMarkerV2(t.Context(), "calendar", "rule", "source")
			if err == nil || event != nil {
				t.Fatalf("accepted invalid lookup: %+v %v", event, err)
			}
			if calls > 250 {
				t.Fatalf("unbounded requests: %d", calls)
			}
		})
	}
}

func TestMarkerLookupSanitizesProviderError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Private": []string{"private-header"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"private-calendar-marker"}}`)), Request: req}, nil
	})}
	svc, err := gcal.NewService(t.Context(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Provider{svc: svc}).FindEventBySyncMarkerV2(t.Context(), "calendar", "rule", "source")
	if err == nil || strings.Contains(err.Error(), "private-calendar-marker") {
		t.Fatalf("unexpected error: %v", err)
	}
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusUnauthorized || apiErr.Message != "" || apiErr.Body != "" || len(apiErr.Header) != 0 || len(apiErr.Errors) != 0 {
		t.Fatalf("provider error was not safely classified: %#v", apiErr)
	}
}
