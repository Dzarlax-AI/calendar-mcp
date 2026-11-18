package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"golang.org/x/oauth2"
)

func TestAuthorityTransportObservesScopedOperationsAndPreservesBody(t *testing.T) {
	cases := []struct {
		method, path, op string
		status           int
		reason           string
	}{
		{"GET", "/calendar/v3/calendars/a%40test/events", "event_list", 404, ""},
		{"GET", "/calendar/v3/calendars/a%40test/events/event", "event_get", 404, ""},
		{"GET", "/calendar/v3/calendars/a%40test/events/event", "event_get", 403, "forbiddenForNonOrganizer"},
		{"PATCH", "/calendar/v3/calendars/a%40test/events/event", "mutation", 403, "forbiddenForNonOrganizer"},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			body := `{"error":{"errors":[{"reason":"` + tc.reason + `"}]}}`
			tr := &authorityTransport{base: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			}), begin: func(_ context.Context, id string) (func(context.Context, string, int, string, string, [32]byte) error, error) {
				if id != "a@test" {
					t.Fatalf("id=%q", id)
				}
				return func(_ context.Context, op string, status int, reason string, _ string, _ [32]byte) error {
					if op != tc.op || status != tc.status || reason != tc.reason {
						t.Fatalf("observation=%s %d %q", op, status, reason)
					}
					return nil
				}, nil
			}}
			req, err := http.NewRequest(tc.method, "https://synthetic.invalid"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(res.Body)
			if err != nil || string(got) != body {
				t.Fatalf("body=%q err=%v", got, err)
			}
		})
	}
}
func TestAuthorityTransportObservesTokenSourceInvalidGrant(t *testing.T) {
	observed := false
	tr := &authorityTransport{base: discoveryRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	}), begin: func(context.Context, string) (func(context.Context, string, int, string, string, [32]byte) error, error) {
		return func(_ context.Context, _ string, status int, reason string, _ string, _ [32]byte) error {
			observed = status == 401 && reason == "invalid_grant"
			return nil
		}, nil
	}}
	req, _ := http.NewRequest("GET", "https://synthetic.invalid/calendar/v3/calendars/a/events", nil)
	_, err := tr.RoundTrip(req)
	var retrieve *oauth2.RetrieveError
	if !observed || !errors.As(err, &retrieve) {
		t.Fatalf("observed=%v err=%v", observed, err)
	}
}

func TestGoogleRequestScopeTreatsResourceWordsAsOpaqueIDs(t *testing.T) {
	for _, id := range []string{"calendarList", "calendars"} {
		req, _ := http.NewRequest("GET", "https://synthetic.invalid/calendar/v3/calendars/"+id+"/events", nil)
		got, op := googleRequestScope(req)
		if got != id || op != "event_list" {
			t.Fatalf("id=%q got=%q op=%q", id, got, op)
		}
	}
	req, _ := http.NewRequest("GET", "https://synthetic.invalid/unrelated/calendars/id/events", nil)
	if id, _ := googleRequestScope(req); id != "" {
		t.Fatal("unrelated endpoint observed")
	}
}

func TestSDKPreservesClosureFailureAfterCancellation(t *testing.T) {
	for _, op := range []string{"list", "get", "events", "sync"} {
		t.Run(op, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			closureErr := errors.New("synthetic closure failure")
			client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
				cancel()
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{}}`)), Request: r}, nil
			})}
			p := &Provider{httpClient: client}
			p.SetDenialObserver(func(context.Context, string) (func(context.Context, string, int, string, string, [32]byte) error, error) {
				return func(context.Context, string, int, string, string, [32]byte) error { return closureErr }, nil
			})
			svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
			if err != nil {
				t.Fatal(err)
			}
			p.svc = svc
			now := time.Now().UTC()
			switch op {
			case "list":
				_, err = p.ListCalendars(ctx)
			case "get":
				_, err = p.Capabilities(ctx, "a")
			case "events":
				_, err = p.GetEvents(ctx, "a", now, now.Add(time.Hour))
			case "sync":
				_, err = p.SyncEvents(ctx, calendar.EventSyncRequest{CalendarID: "a", Window: calendar.EventSyncWindow{Start: now, End: now.Add(time.Hour)}, Mode: calendar.EventSyncReplacement})
			}
			if !errors.Is(err, closureErr) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
