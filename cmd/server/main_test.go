package main

import (
	"net/http"
	"testing"
	"time"

	appruntime "calendar-mcp/internal/runtime"
)

func TestNewHTTPServerHasDefensiveTimeouts(t *testing.T) {
	server := appruntime.NewHTTPServer(":0", http.NewServeMux(), 2*time.Minute)

	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Fatalf("server timeouts must all be positive: %#v", server)
	}
}

func TestNewHTTPServerAllowsStreamingWithoutWriteTimeout(t *testing.T) {
	server := appruntime.NewHTTPServer(":0", http.NewServeMux(), 0)
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want disabled", server.WriteTimeout)
	}
}
