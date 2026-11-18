package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"calendar-mcp/internal/token"
	"golang.org/x/oauth2"
)

// authorityTransport observes bounded denial metadata before exposing an HTTP
// result. It preserves the response body for the generated client.
type authorityTransport struct {
	base    http.RoundTripper
	begin   func(context.Context, string) (func(context.Context, string, int, string, string, [32]byte) error, error)
	binding func() (string, [32]byte)
}

type authorityBody struct {
	io.Reader
	io.Closer
}

func (t *authorityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	id, operation := googleRequestScope(r)
	var finish func(context.Context, string, int, string, string, [32]byte) error
	carrier := token.RequestBindingFromContext(r.Context())
	if carrier == nil {
		carrier = &token.RequestBinding{}
		r = r.WithContext(token.WithRequestBinding(r.Context(), carrier))
	}
	if t.binding != nil {
		carrier.Revision, carrier.Digest = t.binding()
		carrier.Set = true
	}
	if id != "" && t.begin != nil {
		var err error
		finish, err = t.begin(r.Context(), id)
		if err != nil {
			return nil, err
		}
	}
	recordFailure := func(err error) { carrier.AuthorityError = err; log.Print("google authority closure failed") }
	response, err := t.base.RoundTrip(r)
	if finish == nil {
		return response, err
	}
	if err != nil {
		if errors.Is(err, token.ErrAuthorizationScopeChanged) {
			if observeErr := finish(r.Context(), operation, 401, "scope_changed", carrier.Revision, carrier.Digest); observeErr != nil {
				recordFailure(observeErr)
				return nil, observeErr
			}
			return nil, err
		}
		var retrieval *oauth2.RetrieveError
		if errors.As(err, &retrieval) && (retrieval.ErrorCode == "invalid_grant" || (retrieval.Response != nil && retrieval.Response.StatusCode == 401)) {
			if observeErr := finish(r.Context(), operation, 401, "invalid_grant", carrier.Revision, carrier.Digest); observeErr != nil {
				recordFailure(observeErr)
				return nil, observeErr
			}
		}
		return response, err
	}
	if response == nil || response.StatusCode != 401 && response.StatusCode != 403 && response.StatusCode != 404 {
		return response, err
	}
	reason := ""
	if response.StatusCode == 403 && response.Body != nil {
		prefix, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
		response.Body = &authorityBody{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
		if readErr == nil && len(prefix) <= 65536 {
			var body struct {
				Error struct {
					Errors []struct {
						Reason string `json:"reason"`
					} `json:"errors"`
				} `json:"error"`
			}
			if json.Unmarshal(prefix, &body) == nil && len(body.Error.Errors) > 0 && len(body.Error.Errors) <= 32 {
				reasons := make([]string, 0, len(body.Error.Errors))
				valid := true
				for _, item := range body.Error.Errors {
					if item.Reason == "" || len(item.Reason) > 128 || strings.Contains(item.Reason, ",") {
						valid = false
						break
					}
					reasons = append(reasons, item.Reason)
				}
				if valid {
					reason = strings.Join(reasons, ",")
				}
			}
		}
	}
	if observeErr := finish(r.Context(), operation, response.StatusCode, reason, carrier.Revision, carrier.Digest); observeErr != nil {
		recordFailure(observeErr)
		if response.Body != nil {
			response.Body.Close()
		}
		return nil, observeErr
	}
	return response, err
}

func googleRequestScope(r *http.Request) (string, string) {
	if r == nil || r.URL == nil {
		return "", ""
	}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/calendar/v3/")
	if path == r.URL.EscapedPath() {
		return "", ""
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[0] == "users" && parts[1] == "me" && parts[2] == "calendarList" {
		if len(parts) == 3 || (len(parts) == 4 && parts[3] == "") {
			return "@account", "discovery"
		}
		if len(parts) != 4 {
			return "", ""
		}
		id, err := url.PathUnescape(parts[3])
		if err != nil || id == "" {
			return "", ""
		}
		return id, "calendar"
	}
	if len(parts) < 2 || parts[0] != "calendars" {
		return "", ""
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil || id == "" {
		return "", ""
	}
	operation := "calendar"
	if len(parts) >= 3 && parts[2] == "events" {
		operation = "event_get"
		if r.Method == http.MethodGet && (len(parts) == 3 || (len(parts) == 4 && parts[3] == "")) {
			operation = "event_list"
		}
		if r.Method != http.MethodGet {
			operation = "mutation"
		}
	}
	return id, operation
}
