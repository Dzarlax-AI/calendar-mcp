package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

type syntheticGraphReservation struct{}

func (syntheticGraphReservation) ID() string                                { return "synthetic-update-marker" }
func (syntheticGraphReservation) OutcomeDeadline() time.Time                { return time.Now().Add(time.Minute) }
func (syntheticGraphReservation) Authorize(context.Context) error           { return nil }
func (syntheticGraphReservation) Complete(context.Context, any, bool) error { return nil }

type syntheticGraphGuard struct{ dispatchTestGuard }

func (syntheticGraphGuard) BeginGatewayWrite(context.Context, []string, calendar.GatewayWriteScope) (calendar.GatewayWriteReservation, error) {
	return syntheticGraphReservation{}, nil
}

func TestGatewayGraphUpdateUsesOnePatchAndReadOnlyReceipt(t *testing.T) {
	var patches atomic.Int32
	const object = `"id":"object","type":"singleInstance","sensitivity":"normal","start":{"dateTime":"2026-11-18T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-18T10:00:00","timeZone":"UTC"},"attendees":[]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			patches.Add(1)
			var request struct {
				Subject    string                     `json:"subject"`
				Properties []graphSingleValueProperty `json:"singleValueExtendedProperties"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Subject != "updated" ||
				len(request.Properties) != 1 || request.Properties[0].ID != microsoftGatewayWriteProperty ||
				request.Properties[0].Value != (syntheticGraphReservation{}).ID() {
				t.Errorf("tracked update marker did not accompany the original patch: %v", err)
			}
			http.Error(w, "synthetic lost response", http.StatusServiceUnavailable)
		case http.MethodGet:
			if strings.Contains(r.URL.Query().Get("$expand"), microsoftGatewayWriteProperty) && patches.Load() == 1 {
				fmt.Fprintf(w, `{%s,"subject":"updated","singleValueExtendedProperties":[{"id":%q,"value":%q}]}`, object, microsoftGatewayWriteProperty, (syntheticGraphReservation{}).ID())
			} else {
				fmt.Fprintf(w, `{%s,"subject":"original"}`, object)
			}
		default:
			t.Errorf("unexpected request method %s", r.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	p := &Provider{baseURL: server.URL, client: server.Client()}
	ref := calendar.EventRef{CalendarID: "calendar", EventID: "object"}
	ctx, _ := calendar.EnableGatewayWriteTracking(t.Context())
	calendar.DescribeGatewayWrite(ctx, p, calendar.GatewayWriteScope{Kind: "update", CalendarID: "calendar", EventID: "object", Scope: calendar.ScopeSeries})
	_, err := calendar.GuardedWrite(ctx, &syntheticGraphGuard{}, []string{"calendar"}, func() (*calendar.OperationResult, error) {
		return p.UpdateEventV2(ctx, calendar.UpdateEventRequestV2{Ref: ref, Scope: calendar.ScopeSeries, Notifications: calendar.NotificationsNone,
			Patch: calendar.EventPatchV2{Title: calendar.PatchField[string]{Value: "updated", Present: true}}})
	}, func(result *calendar.OperationResult) bool { return result != nil })
	if !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) {
		t.Fatalf("mutation should remain unresolved: %v", err)
	}
	found, err := p.FindGatewayPointWrite(ctx, ref, (syntheticGraphReservation{}).ID())
	if err != nil || found == nil || found.Title != "updated" || patches.Load() != 1 {
		t.Fatalf("read-only receipt check failed: found=%v patches=%d err=%v", found != nil, patches.Load(), err)
	}
	if wrong, err := p.FindGatewayPointWrite(ctx, ref, "another-marker"); err != nil || wrong != nil {
		t.Fatal("incorrect marker accepted as a receipt")
	}
}
