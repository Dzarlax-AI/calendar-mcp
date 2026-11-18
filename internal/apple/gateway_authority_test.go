package apple

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestGatewayEvidenceRequiresExplicitCalDAVPrivileges(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "0" {
			t.Errorf("unexpected privilege request: %s", r.Method)
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprintf(w, `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
<D:response><D:href>%s</D:href>
<D:propstat><D:prop><D:owner><D:href>/principals/owner/</D:href></D:owner><D:current-user-privilege-set>
<D:privilege><C:read-free-busy/></D:privilege>%s
</D:current-user-privilege-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
</D:response></D:multistatus>`, r.URL.Path, map[string]string{"/owned/": "<D:privilege><D:read/></D:privilege><D:privilege><D:write/></D:privilege>"}[r.URL.Path])
	}))
	defer s.Close()
	p := eventSyncProvider(t, s)
	busyOnly, err := p.gatewayCollectionAccess(t.Context(), "/busy/")
	if err != nil || !busyOnly.known || busyOnly.read || busyOnly.write {
		t.Fatalf("free-busy privileges=%#v, error=%v", busyOnly, err)
	}
	owned, err := p.gatewayCollectionAccess(t.Context(), "/owned/")
	if err != nil || !owned.known || !owned.read || !owned.write || !appleSameCollectionPrincipal(s.URL, owned.owner, "/principals/owner/") {
		t.Fatalf("owner privileges=%#v, error=%v", owned, err)
	}
	if appleSameCollectionPrincipal(s.URL, owned.owner, "https://example.invalid/principals/owner/") {
		t.Fatal("owner with a different origin accepted")
	}
}

type gatewayAppleRoundTrip func(*http.Request) (*http.Response, error)

func (f gatewayAppleRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayDiscoveryRawUnauthorizedIsDefinitive(t *testing.T) {
	p, err := New("fixture", "fixture", "https://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`<D:multistatus xmlns:D="DAV:"/>`)), Request: r}, nil
	})
	_, err = p.CheckCalendarReadEvidence(t.Context())
	var api *calendar.APIError
	if !errors.As(err, &api) || api.Code != calendar.ErrorPermissionDenied {
		t.Fatalf("expected definitive authentication denial, got %v", err)
	}
}

func TestGatewayObservedOwnerDowngradeSurvivesUnavailableProbe(t *testing.T) {
	p, err := New("fixture", "fixture", "https://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	var restricted bool
	ctx := calendar.WithGatewayReadRestriction(t.Context(), func(_ context.Context, id string, denied bool) error {
		if id != "/shared/" || denied {
			t.Fatalf("unexpected restriction: %s %t", id, denied)
		}
		restricted = true
		return nil
	})
	p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusServiceUnavailable, ""
		if r.Method == "PROPFIND" && r.Header.Get("Depth") == "0" {
			status, body = http.StatusMultiStatus, `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/shared/</D:href>
<D:propstat><D:prop><D:owner><D:href>/other/</D:href></D:owner><D:current-user-privilege-set>
<D:privilege><D:read/></D:privilege></D:current-user-privilege-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
</D:response></D:multistatus>`
		} else if !restricted {
			t.Fatal("event probe started before the observed downgrade was committed")
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	proof, err := p.checkGatewayCollection(ctx, ctx, &discoveryStatus{}, calendar.Calendar{ID: "/shared/"}, "/self/", time.Now())
	if err != nil || !restricted || !proof.Unknown {
		t.Fatalf("proof=%+v restriction=%t err=%v", proof, restricted, err)
	}
}
