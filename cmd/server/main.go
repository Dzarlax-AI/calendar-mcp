package main

import (
	"context"
	"log"
	"net/http"
	"time"

	appruntime "calendar-mcp/internal/runtime"
)

func main() {
	if err := appruntime.Serve(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func newHTTPServer(addr string, handler http.Handler, writeTimeout time.Duration) *http.Server {
	return appruntime.NewHTTPServer(addr, handler, writeTimeout)
}

func runServers(servers []*http.Server) error {
	return appruntime.RunServers(servers)
}
