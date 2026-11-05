package google

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"calendar-mcp/internal/calendar"
)

func TestSafeTargetFullGETAllowsOmittedEmptyAttendeeField(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		t.Run(op, func(t *testing.T) {
			writes := 0
			p := markerTestProvider(t, func(r *http.Request) string {
				if r.Method == http.MethodGet {
					if r.URL.Query().Get("maxAttendees") != "" || r.URL.Query().Get("fields") != "" {
						t.Error("partial GET")
					}
					return `{"id":"event","eventType":"default","etag":"\"v1\"","organizer":{"self":true},"start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`
				}
				writes++
				if r.Header.Get("If-Match") != `"v1"` || r.URL.Query().Get("sendUpdates") != "none" {
					t.Error("missing conditional no-notification guard")
				}
				if op == "update" {
					b, _ := io.ReadAll(r.Body)
					var obj map[string]any
					if json.Unmarshal(b, &obj) != nil {
						t.Error("invalid JSON")
					}
					if _, ok := obj["attendees"]; ok {
						t.Error("attendees included in update")
					}
				}
				return `{"id":"event","start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}}`
			})
			ref := calendar.EventRef{CalendarID: "primary", EventID: "event"}
			var err error
			if op == "update" {
				_, err = p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
			} else {
				_, err = p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone, RequireNoAttendees: true})
			}
			if err != nil || writes != 1 {
				t.Fatalf("write: %d %v", writes, err)
			}
		})
	}
}
