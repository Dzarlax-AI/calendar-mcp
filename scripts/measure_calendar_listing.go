//go:build ignore

// Synthetic loopback listing benchmark; no real providers or credentials.
package main

import (
	"calendar-mcp/internal/apple"
	"calendar-mcp/internal/application"
	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/mcpserver"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"time"
)

type delayed struct {
	calendar.Provider
	name  string
	delay time.Duration
}

func (p *delayed) Name() string { return p.name }
func (p *delayed) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	select {
	case <-time.After(p.delay):
		return []calendar.Calendar{{ID: "one", Name: p.name}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func main() {
	serve := flag.Bool("serve", false, "serve synthetic calendar MCP for integration")
	flag.Parse()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var prop string
		var delay time.Duration
		switch r.URL.Path {
		case "/":
			delay = 28 * time.Millisecond
			prop = `<d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal>`
		case "/principal/":
			delay = 26 * time.Millisecond
			prop = `<c:calendar-home-set><d:href>/home/</d:href></c:calendar-home-set>`
		case "/home/":
			delay = 36 * time.Millisecond
			prop = `<d:resourcetype><c:calendar/></d:resourcetype><d:displayname>synthetic</d:displayname>`
		default:
			panic("unexpected synthetic path")
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, prop)
	}))
	defer server.Close()
	newRegistry := func() *calendar.Registry {
		p, e := apple.New("synthetic", "synthetic", server.URL)
		if e != nil {
			panic(e)
		}
		return calendar.NewRegistry([]calendar.Provider{p, &delayed{name: "google_a", delay: 10 * time.Millisecond}, &delayed{name: "google_b", delay: 41 * time.Millisecond}})
	}
	if *serve {
		reg := newRegistry()
		mux := http.NewServeMux()
		mcpserver.Register(mux, reg, application.New(reg), auth.Options{AllowUnauthenticated: true}, false)
		mux.HandleFunc("GET /fixture/metrics", func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"apple_http_requests": calls.Load()})
		})
		front := httptest.NewServer(mux)
		defer front.Close()
		json.NewEncoder(os.Stdout).Encode(map[string]any{"endpoint": front.URL + "/mcp", "metrics": front.URL + "/fixture/metrics"})
		select {}
	}
	rows := []map[string]any{}
	var warm *calendar.Registry
	for _, mode := range []string{"cold", "warm"} {
		for run := 1; run <= 5; run++ {
			reg := warm
			if mode == "cold" || reg == nil {
				reg = newRegistry()
				if mode == "warm" {
					warm = reg
					if _, e := reg.ListCalendars(context.Background()); e != nil {
						panic(e)
					}
				}
			}
			before := calls.Load()
			t := time.Now()
			c, e := reg.ListCalendars(context.Background())
			ms := float64(time.Since(t).Microseconds()) / 1000
			if e != nil || len(c) != 3 || c[0].Provider != "apple" || c[1].Provider != "google_a" || c[2].Provider != "google_b" {
				panic("listing parity failed")
			}
			rows = append(rows, map[string]any{"mode": mode, "run": run, "ms": ms, "apple_http_requests": calls.Load() - before, "calendars": len(c), "stable_order": true})
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"synthetic": true, "delays_ms": []int{28, 26, 36, 10, 41}, "rows": rows})
}
