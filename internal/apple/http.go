package apple

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"time"
)

type gatewayRawStatusKey struct{}

type basicAuthTransport struct {
	username string
	password string
	base     http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.SetBasicAuth(t.username, t.password)
	resp, err := t.base.RoundTrip(req2)
	if err != nil {
		return nil, err
	}
	if status, ok := req.Context().Value(gatewayRawStatusKey{}).(*discoveryStatus); ok && resp.StatusCode == http.StatusUnauthorized {
		status.code.CompareAndSwap(0, int32(resp.StatusCode))
	}
	// Preserve the original status for listing only. Event fallback behavior is unchanged.
	if status, ok := req.Context().Value(discoveryStatusKey{}).(*discoveryStatus); ok {
		switch resp.StatusCode {
		case 401, 403, 404, 410:
			status.code.CompareAndSwap(0, int32(resp.StatusCode))
		}
	}
	// Apple CalDAV sometimes returns <multistatus> with HTTP 4xx/5xx.
	// go-webdav treats non-2xx as error and tries to parse <error> XML,
	// which fails. Fix: if body contains <multistatus>, force 207.
	// We read the entire body to avoid truncation on streaming responses.
	if resp.StatusCode >= 400 {
		origStatus := resp.StatusCode
		limit := 0
		if _, strict := req.Context().Value(gatewayRawStatusKey{}).(*discoveryStatus); strict {
			limit = maxGatewayDiscoveryBytes
		}
		body, ok, err := readIfMultiStatus(resp, limit)
		if err != nil {
			return nil, err
		}
		if ok {
			// Do not log the CalDAV path: it can contain account and calendar
			// identifiers. The status and bounded body length are sufficient for
			// diagnosing the fallback.
			log.Printf("apple: CalDAV status=%d → forcing 207 (multistatus body, %d bytes)", origStatus, len(body))
			resp.StatusCode = 207
			resp.Status = "207 Multi-Status"
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	return resp, nil
}

// readIfMultiStatus reads the full response body. If it contains a multistatus
// element, returns the body bytes and true. Otherwise restores the body and
// returns false so the caller can process the response normally.
func readIfMultiStatus(resp *http.Response, limit int) ([]byte, bool, error) {
	if resp.Body == nil {
		return nil, false, nil
	}
	var reader io.Reader = resp.Body
	if limit > 0 {
		reader = io.LimitReader(resp.Body, int64(limit)+1)
	}
	body, err := io.ReadAll(reader)
	resp.Body.Close()
	if limit > 0 && len(body) > limit {
		return nil, false, errors.New("CalDAV authority response exceeds read bounds")
	}
	if err != nil || len(body) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil, false, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if bytes.Contains(body, []byte("multistatus")) {
		return body, true, nil
	}
	return nil, false, nil
}

func newBasicAuthClient(username, password string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &basicAuthTransport{
			username: username,
			password: password,
			base:     http.DefaultTransport,
		},
	}
}
