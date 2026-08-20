package main

import (
	"context"
	"log"

	appruntime "calendar-mcp/internal/runtime"
)

func main() {
	if err := appruntime.Serve(context.Background()); err != nil {
		log.Fatal(err)
	}
}
