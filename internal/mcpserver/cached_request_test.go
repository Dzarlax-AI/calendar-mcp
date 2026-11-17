package mcpserver

import (
	"strings"
	"testing"
)

func TestCachedArgumentsRejectDuplicateUnknownAndWrongTypes(t *testing.T) {
	base := `"calendar_ids":["google:account:primary"],"start":"2026-11-17T10:00:00Z","end":"2026-11-17T11:00:00Z"`
	for _, raw := range []string{
		`{` + base + `,"start":"2026-11-17T12:00:00Z"}`,
		`{` + base + `,"caller":"spoof"}`,
		`{` + base + `,"page_size":1.5}`,
		`{` + base + `,"page_size":0}`,
		`{` + base + `,"page_size":null}`,
		`{` + base + `,"page_token":null}`,
		`{` + base + `,"page_token":"` + strings.Repeat("x", 4097) + `"}`,
		`{` + base + `} {}`,
		`{"calendar_ids":[],"start":false,"end":"2026-11-17T11:00:00Z"}`,
	} {
		if _, _, err := parseCachedArguments([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid args: %.160s", raw)
		}
	}
	request, token, err := parseCachedArguments([]byte(`{` + base + `}`))
	if err != nil || request.PageSize != 25 || token != "" || len(request.CalendarIDs) != 1 {
		t.Fatalf("default parse: %+v %q %v", request, token, err)
	}
}
