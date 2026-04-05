package apple

import "net/http"

type basicAuthTransport struct {
	username string
	password string
	base     http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.SetBasicAuth(t.username, t.password)
	return t.base.RoundTrip(req2)
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
