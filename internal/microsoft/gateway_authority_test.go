package microsoft

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestGatewayEvidenceSeparatesMetadataReadAndPrivateAccess(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation method %s", r.Method)
		}
		switch r.URL.Path {
		case "/me/calendars":
			fmt.Fprint(w, `{"value":[{"id":"open","name":"Open","canEdit":true,"canViewPrivateItems":true},{"id":"ordinary","name":"Ordinary"},{"id":"denied","name":"Denied","canViewPrivateItems":true}]}`)
		case "/me/calendars/open/calendarView", "/me/calendars/ordinary/calendarView":
			if r.URL.Query().Get("$top") != "1" || r.URL.Query().Get("$select") != "id" {
				t.Error("expected bounded event access check")
			}
			fmt.Fprint(w, `{"value":[]}`)
		case "/me/calendars/denied/calendarView":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":"ErrorAccessDenied"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer s.Close()
	p := &Provider{baseURL: s.URL, client: s.Client()}
	proofs, err := p.CheckCalendarReadEvidence(t.Context())
	if err != nil || len(proofs) != 3 {
		t.Fatalf("proof count=%d, error=%v", len(proofs), err)
	}
	if !proofs[0].ReadPrivate || !proofs[0].ReadEvents || !proofs[0].Capabilities.Operations.Update {
		t.Fatal("explicit private and edit access lost")
	}
	if !proofs[1].ReadEvents || proofs[1].ReadPrivate || proofs[1].Capabilities.Operations.Update {
		t.Fatal("missing properties inferred stronger authority")
	}
	if proofs[2].ReadEvents || proofs[2].ReadPrivate || proofs[2].Capabilities.Operations.Get {
		t.Fatal("denied event probe retained event authority")
	}
}

type gatewayRoundTripFunc func(*http.Request) (*http.Response, error)

func (f gatewayRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayEvidenceContinuesAfterTransientCalendarProbe(t *testing.T) {
	client := &http.Client{Transport: gatewayRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, `{"value":[]}`
		switch r.URL.Path {
		case "/me/calendars":
			body = `{"value":[{"id":"unknown"},{"id":"healthy","canViewPrivateItems":true}]}`
		case "/me/calendars/unknown/calendarView":
			status, body = http.StatusServiceUnavailable, `{}`
		case "/me/calendars/healthy/calendarView":
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	p := &Provider{baseURL: "https://example.invalid", client: client}
	proofs, err := p.CheckCalendarReadEvidence(t.Context())
	if err != nil || len(proofs) != 2 {
		t.Fatalf("proofs=%v err=%v", proofs, err)
	}
	if !proofs[0].Unknown || proofs[1].Unknown || !proofs[1].ReadPrivate {
		t.Fatalf("unexpected evidence: %+v", proofs)
	}
}

func TestGatewaySlowCalendarProbeLeavesTimeForSibling(t *testing.T) {
	client := &http.Client{Transport: gatewayRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"value":[]}`
		switch r.URL.Path {
		case "/me/calendars":
			body = `{"value":[{"id":"slow","canViewPrivateItems":true},{"id":"healthy","canViewPrivateItems":true}]}`
		case "/me/calendars/slow/calendarView":
			<-r.Context().Done()
			return nil, r.Context().Err()
		case "/me/calendars/healthy/calendarView":
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	p := &Provider{baseURL: "https://example.invalid", client: client}
	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	proofs, err := p.CheckCalendarReadEvidence(ctx)
	if err != nil || len(proofs) != 2 || !proofs[0].Unknown || !proofs[1].ReadPrivate {
		t.Fatalf("unexpected evidence: %+v, err=%v", proofs, err)
	}
}

func TestGatewayRestrictsPrivateAccessBeforeFollowingInventoryPage(t *testing.T) {
	var restricted bool
	client := &http.Client{Transport: gatewayRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/me/calendars" || r.Method != http.MethodGet {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		status, body := http.StatusOK, `{"value":[{"id":"changed","canViewPrivateItems":false}],"@odata.nextLink":"https://example.invalid/me/calendars?page=2"}`
		if r.URL.Query().Get("page") == "2" {
			if !restricted {
				t.Fatal("second page requested before confirmed downgrade was applied")
			}
			status, body = http.StatusServiceUnavailable, `{"error":{"code":"ServiceUnavailable"}}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	p := &Provider{baseURL: "https://example.invalid", client: client}
	ctx := calendar.WithGatewayReadRestriction(t.Context(), func(_ context.Context, id string, denied bool) error {
		if id != "changed" || denied {
			t.Fatalf("unexpected restriction: %s denied=%t", id, denied)
		}
		restricted = true
		return nil
	})
	result, err := p.CheckCalendarReadEvidence(ctx)
	if err == nil || result != nil || !restricted {
		t.Fatalf("result=%v restricted=%t err=%v", result, restricted, err)
	}
}
