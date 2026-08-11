package google

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"calendar-mcp/internal/calendar"
)

func TestToGoogleEventTime_AllDayUsesDate(t *testing.T) {
	tm := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	got := toGoogleEventTime(tm, true)
	if got.Date != "2026-05-30" {
		t.Fatalf("Date = %q, want 2026-05-30", got.Date)
	}
	if got.DateTime != "" {
		t.Fatalf("DateTime = %q, want empty", got.DateTime)
	}
}

func TestToGoogleEventTime_TimedUsesDateTime(t *testing.T) {
	tm := time.Date(2026, 5, 30, 13, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	got := toGoogleEventTime(tm, false)
	if got.Date != "" {
		t.Fatalf("Date = %q, want empty", got.Date)
	}
	if got.DateTime != "2026-05-30T13:00:00+02:00" {
		t.Fatalf("DateTime = %q, want 2026-05-30T13:00:00+02:00", got.DateTime)
	}
}

func TestListCalendarsFollowsPagination(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "next" {
			_, _ = io.WriteString(w, `{"items":[{"id":"second","summary":"Second"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"first","summary":"First"}],"nextPageToken":"next"}`)
	})
	defer closeServer()

	got, err := provider.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("ListCalendars() = %#v, want both pages", got)
	}
}

func TestGetEventsFollowsPagination(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "next" {
			_, _ = io.WriteString(w, `{"items":[{"id":"second","summary":"Second","start":{"dateTime":"2026-08-10T11:00:00Z"},"end":{"dateTime":"2026-08-10T12:00:00Z"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"first","summary":"First","start":{"dateTime":"2026-08-10T09:00:00Z"},"end":{"dateTime":"2026-08-10T10:00:00Z"}}],"nextPageToken":"next"}`)
	})
	defer closeServer()

	got, err := provider.GetEvents(context.Background(), "primary", time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GetEvents() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("GetEvents() = %#v, want both pages", got)
	}
}

func TestLegacyWritesDisableNotifications(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Provider) error
	}{
		{
			name: "create",
			run: func(ctx context.Context, p *Provider) error {
				_, err := p.CreateEvent(ctx, "primary", calendar.EventCreate{
					Title: "Safe create",
					Start: time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
					End:   time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC),
				})
				return err
			},
		},
		{
			name: "update",
			run: func(ctx context.Context, p *Provider) error {
				title := "Updated"
				_, err := p.UpdateEvent(ctx, "primary", "event", calendar.EventUpdate{Title: &title})
				return err
			},
		},
		{
			name: "delete",
			run: func(ctx context.Context, p *Provider) error {
				return p.DeleteEvent(ctx, "primary", "event")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"id":"event","summary":"Existing","start":{"dateTime":"2026-08-10T09:00:00Z"},"end":{"dateTime":"2026-08-10T10:00:00Z"}}`)
					return
				}
				if got := r.URL.Query().Get("sendUpdates"); got != "none" {
					t.Errorf("sendUpdates = %q, want none", got)
				}
				if r.Method != http.MethodDelete {
					_, _ = io.WriteString(w, `{"id":"event","summary":"Saved","start":{"dateTime":"2026-08-10T09:00:00Z"},"end":{"dateTime":"2026-08-10T10:00:00Z"}}`)
				}
			})
			defer closeServer()

			if err := tt.run(context.Background(), provider); err != nil {
				t.Fatalf("write error = %v", err)
			}
		})
	}
}

func TestUpdateEventCanClearAllAttendees(t *testing.T) {
	provider, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"id":"event","summary":"Existing","attendees":[{"email":"person@example.com"}],"start":{"dateTime":"2026-08-10T09:00:00Z"},"end":{"dateTime":"2026-08-10T10:00:00Z"}}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		if !bytes.Contains(body, []byte(`"attendees":[]`)) {
			t.Errorf("request body %s does not explicitly clear attendees", body)
		}
		_, _ = io.WriteString(w, `{"id":"event","summary":"Existing","start":{"dateTime":"2026-08-10T09:00:00Z"},"end":{"dateTime":"2026-08-10T10:00:00Z"}}`)
	})
	defer closeServer()
	empty := []calendar.Attendee{}

	if _, err := provider.UpdateEvent(context.Background(), "primary", "event", calendar.EventUpdate{Attendees: &empty}); err != nil {
		t.Fatalf("UpdateEvent() error = %v", err)
	}
}

func testProvider(t *testing.T, handler http.HandlerFunc) (*Provider, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	svc, err := gcal.NewService(context.Background(), option.WithoutAuthentication(), option.WithEndpoint(server.URL+"/"))
	if err != nil {
		server.Close()
		t.Fatalf("NewService() error = %v", err)
	}
	return &Provider{svc: svc}, server.Close
}
