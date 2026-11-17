package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"calendar-mcp/internal/storage"
	"github.com/mark3labs/mcp-go/mcp"
)

type cachedBodyKey struct{}
type cachedRequestBody struct {
	io.ReadCloser
	raw []byte
}

func (b *cachedRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.raw = append(b.raw, p[:n]...)
	return n, err
}

// The SDK decodes arguments into a map, discarding duplicate JSON keys. Keep
// the bytes it actually reads under the existing outer HTTP body limit. This
// preserves SDK parsing/error/session behavior and only affects the new tool.
func observeCachedRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Body != nil {
			body := &cachedRequestBody{ReadCloser: r.Body}
			r = r.WithContext(context.WithValue(r.Context(), cachedBodyKey{}, body))
			r.Body = body
		}
		next.ServeHTTP(w, r)
	})
}

func cachedArgumentsJSON(ctx context.Context, request mcp.CallToolRequest) ([]byte, error) {
	if body, ok := ctx.Value(cachedBodyKey{}).(*cachedRequestBody); ok {
		var envelope struct {
			Params struct {
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body.raw, &envelope); err != nil {
			return nil, err
		}
		return envelope.Params.Arguments, nil
	}
	return json.Marshal(request.GetRawArguments())
}

var errCachedArguments = errors.New("invalid cached event arguments")

func parseCachedArguments(raw []byte) (storage.CachedPageRequest, string, error) {
	var req storage.CachedPageRequest
	if !utf8.Valid(raw) {
		return req, "", errCachedArguments
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return req, "", errCachedArguments
	}
	fields := make(map[string]json.RawMessage, 6)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return req, "", errCachedArguments
		}
		if _, duplicate := fields[name]; duplicate {
			return req, "", errCachedArguments
		}
		switch name {
		case "calendar_ids", "start", "end", "page_size", "page_token":
		default:
			return req, "", errCachedArguments
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || bytes.Equal(value, []byte("null")) {
			return req, "", errCachedArguments
		}
		fields[name] = value
	}
	if last, err := decoder.Token(); err != nil || last != json.Delim('}') {
		return req, "", errCachedArguments
	}
	if _, err = decoder.Token(); err != io.EOF {
		return req, "", errCachedArguments
	}
	if err = json.Unmarshal(fields["calendar_ids"], &req.CalendarIDs); err != nil {
		return req, "", errCachedArguments
	}
	for name, target := range map[string]*time.Time{"start": &req.Start, "end": &req.End} {
		var value string
		if err = json.Unmarshal(fields[name], &value); err != nil || len(value) > 128 {
			return req, "", errCachedArguments
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return req, "", errCachedArguments
		}
		*target = parsed
	}
	req.PageSize = 25
	if value, ok := fields["page_size"]; ok {
		if err = json.Unmarshal(value, &req.PageSize); err != nil || req.PageSize < 1 || req.PageSize > 50 {
			return req, "", errCachedArguments
		}
	}
	var pageToken string
	if value, ok := fields["page_token"]; ok {
		if err = json.Unmarshal(value, &pageToken); err != nil || len(pageToken) > 4096 {
			return req, "", errCachedArguments
		}
	}
	return req, pageToken, nil
}
