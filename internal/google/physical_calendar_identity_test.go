package google

import (
	"io"
	"net/http"
	"testing"
)

func TestPhysicalCalendarIdentityAliases(t *testing.T) {
	p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		if r.URL.Path != "/users/me/calendarList/primary" && r.URL.Path != "/users/me/calendarList/owner@example.invalid" {
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"owner@example.invalid","primary":true}`)
	})
	defer closeServer()
	primary, err := p.ResolvePhysicalCalendarKey(t.Context(), "primary")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := p.ResolvePhysicalCalendarKey(t.Context(), "owner@example.invalid")
	if err != nil || primary != alias {
		t.Fatalf("alias mismatch: %v", err)
	}
}

func TestPhysicalCalendarIdentityRejectsUnverifiedResponse(t *testing.T) {
	for _, body := range []string{`{}`, `{"id":"primary","primary":true}`, `{"id":"owner@example.invalid"}`, `{"id":"owner@example.invalid","primary":true,"deleted":true}`, `{"id":" owner@example.invalid","primary":true}`} {
		t.Run(body, func(t *testing.T) {
			p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			})
			defer closeServer()
			if _, err := p.ResolvePhysicalCalendarKey(t.Context(), "primary"); err == nil {
				t.Fatal("accepted unverifiable identity")
			}
		})
	}
}
