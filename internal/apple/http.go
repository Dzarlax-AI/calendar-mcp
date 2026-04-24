package apple

import (
	"bytes"
	"io"
	"log"
	"net/http"
)

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
	// Apple CalDAV sometimes returns <multistatus> with HTTP 4xx/5xx.
	// go-webdav treats non-2xx as error and tries to parse <error> XML,
	// which fails. Fix: if body contains <multistatus>, force 207.
	// We read the entire body to avoid truncation on streaming responses.
	if resp.StatusCode >= 400 {
		origStatus := resp.StatusCode
		if body, ok := readIfMultiStatus(resp); ok {
			log.Printf("apple: CalDAV %s %d → forcing 207 (multistatus body, %d bytes)", req.URL.Path, origStatus, len(body))
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
func readIfMultiStatus(resp *http.Response) ([]byte, bool) {
	if resp.Body == nil {
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(body) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil, false
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if bytes.Contains(body, []byte("multistatus")) {
		return body, true
	}
	return nil, false
}

func newBasicAuthClient(username, password string) *http.Client {
	return &http.Client{
		Transport: &basicAuthTransport{
			username: username,
			password: password,
			base:     http.DefaultTransport,
		},
	}
}
