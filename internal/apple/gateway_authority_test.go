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
</D:response></D:multistatus>`, r.URL.EscapedPath(), map[string]string{"/owned/": "<D:privilege><D:read/></D:privilege><D:privilege><D:write/></D:privilege>"}[r.URL.Path])
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
	if _, err := p.gatewayCollectionAccess(t.Context(), "/home/team#ops?/"); err != nil {
		t.Fatalf("escaped discovery path was rejected: %v", err)
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

func TestGatewayOwnedPrivateAccessProbesObjectWhenReportEmpty(t *testing.T) {
	p, err := New("fixture", "fixture", "https://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	var objectReads int
	p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, media := http.StatusMultiStatus, "application/xml"
		const open = `<D:multistatus xmlns:D="DAV:">`
		const close = `</D:multistatus>`
		var body string
		switch {
		case r.Method == "PROPFIND" && r.Header.Get("Depth") == "0":
			body = open + `<D:response><D:href>/home/team/</D:href><D:propstat><D:prop>
<D:owner><D:href>/owner/</D:href></D:owner><D:current-user-privilege-set><D:privilege><D:read/></D:privilege></D:current-user-privilege-set>
</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` + close
		case r.Method == "REPORT":
			body = open + close
		case r.Method == "PROPFIND" && r.Header.Get("Depth") == "1":
			body = open + `<D:response><D:href>/home/team/event.ics</D:href><D:propstat><D:prop><D:getetag>"event"</D:getetag></D:prop>
<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` + close
		case r.Method == "GET":
			if r.URL.EscapedPath() != "/home/team/event.ics" {
				t.Fatalf("object path = %s", r.URL.EscapedPath())
			}
			objectReads++
			status, media = http.StatusOK, "text/calendar"
			body = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:private\r\nDTSTAMP:20261101T090000Z\r\nDTSTART:20261101T101000Z\r\nCLASS:PRIVATE\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		default:
			t.Fatalf("unexpected request method or depth: %s %s", r.Method, r.Header.Get("Depth"))
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{media}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	proof, err := p.checkGatewayCollection(t.Context(), t.Context(), &discoveryStatus{}, calendar.Calendar{ID: "/home/team/"}, "/owner/", time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	if err != nil || proof.Unknown || !proof.ReadEvents || !proof.ReadPrivate || objectReads != 1 {
		t.Fatalf("proof=%+v object reads=%d err=%v", proof, objectReads, err)
	}
}

func TestGatewayWriteRejectsUppercaseCancelledReceipt(t *testing.T) {
	p, err := New("fixture", "fixture", "https://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, media := http.StatusMultiStatus, "application/xml"
		body := `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/home/owned/event.ics</D:href></D:response></D:multistatus>`
		switch r.Method {
		case "PROPFIND":
		case "GET":
			status, media = http.StatusOK, "text/calendar"
			body = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:cancelled\r\nDTSTAMP:20261101T090000Z\r\nDTSTART:20261101T101000Z\r\nSTATUS:CANCELLED\r\nX-CALENDAR-MCP-WRITE-DISPATCH:dispatch-id\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{media}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	if receipt, err := p.FindGatewayWrite(t.Context(), "/home/owned/", "dispatch-id"); receipt != nil || err == nil {
		t.Fatalf("cancelled object was accepted as confirmation: %+v error=%v", receipt, err)
	}
}

func TestGatewayEmptyOwnedCalendarCanReadWithoutPrivateEvidence(t *testing.T) {
	p, err := New("fixture", "fixture", "https://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	const collection = "/home/owned/"
	const open = `<D:multistatus xmlns:D="DAV:">`
	const closeTag = `</D:multistatus>`
	p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, media := http.StatusMultiStatus, "application/xml"
		body := open + closeTag
		switch {
		case r.Method == "PROPFIND" && r.Header.Get("Depth") == "0":
			body = open + `<D:response><D:href>/home/owned/</D:href><D:propstat><D:prop>
<D:owner><D:href>/owner/</D:href></D:owner><D:current-user-privilege-set><D:privilege><D:read/></D:privilege></D:current-user-privilege-set>
</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` + closeTag
		case r.Method == "REPORT":
		case r.Method == "PROPFIND" && r.Header.Get("Depth") == "1":
			body = open + `<D:response><D:href>/home/owned/task.ics</D:href></D:response>` + closeTag
		case r.Method == "GET" && r.URL.Path == collection+"task.ics":
			status, media = http.StatusOK, "text/calendar"
			body = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VTODO\r\nUID:task\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {media}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	proof, err := p.checkGatewayCollection(t.Context(), t.Context(), &discoveryStatus{},
		calendar.Calendar{ID: collection}, "/owner/", time.Now())
	if err != nil || proof.Unknown || !proof.ReadEvents || proof.ReadPrivate {
		t.Fatalf("empty calendar proof=%+v err=%v", proof, err)
	}
}

func TestGatewayStrictAppleListingFallsBackOnRaw500Only(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p, err := New("fixture", "fixture", "https://example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			var inventoryCalls int
			p.httpClient.Transport.(*basicAuthTransport).base = gatewayAppleRoundTrip(func(r *http.Request) (*http.Response, error) {
				switch r.Method {
				case "REPORT":
					body := `<D:error xmlns:D="DAV:">`
					if status == http.StatusForbidden {
						body = `<D:multistatus xmlns:D="DAV:"/>`
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				case "PROPFIND":
					inventoryCalls++
					return &http.Response{StatusCode: http.StatusMultiStatus, Body: io.NopCloser(strings.NewReader(
						`<D:multistatus xmlns:D="DAV:"/>`))}, nil
				default:
					t.Fatalf("unexpected %s", r.Method)
					return nil, nil
				}
			})
			now := time.Now()
			page, err := p.ListEventsV2(t.Context(), calendar.ListEventsRequestV2{CalendarID: "/owned/",
				Start: now, End: now.Add(time.Hour), View: calendar.RecurrenceBoth, AvailabilityStrict: true})
			if status == http.StatusInternalServerError {
				if err != nil || !page.Complete || inventoryCalls != 1 {
					t.Fatalf("page=%+v err=%v inventory=%d", page, err, inventoryCalls)
				}
				return
			}
			var denied *calendar.EventSyncError
			if !errors.As(err, &denied) || denied.Class != calendar.EventSyncPermission || inventoryCalls != 0 {
				t.Fatalf("err=%v inventory=%d", err, inventoryCalls)
			}
		})
	}
}
