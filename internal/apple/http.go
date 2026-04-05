package apple

import (
	"bytes"
	"io"
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
	// Apple CalDAV sometimes returns <multistatus> with HTTP 500.
	// go-webdav treats non-2xx as error and tries to parse <error> XML,
	// which fails. Fix: if body starts with <multistatus>, force 207.
	if resp.StatusCode >= 400 && isMultiStatus(resp) {
		resp.StatusCode = 207
		resp.Status = "207 Multi-Status"
	}
	return resp, nil
}

func isMultiStatus(resp *http.Response) bool {
	if resp.Body == nil {
		return false
	}
	// Peek at the first 512 bytes
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if n == 0 {
		return false
	}
	// Reconstruct body with peeked data
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf[:n]), resp.Body))
	return bytes.Contains(buf[:n], []byte("multistatus"))
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
