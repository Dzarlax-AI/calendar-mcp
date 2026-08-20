.PHONY: assets build test test-integration vet html-check image

GOCACHE ?= /tmp/calendar-mcp-go-build

assets:
	./scripts/fetch-htmx.sh

build: assets
	GOCACHE=$(GOCACHE) CGO_ENABLED=0 go build -o calendar ./cmd/calendar

test:
	GOCACHE=$(GOCACHE) go test -race -count=1 ./...

test-integration:
	GOCACHE=$(GOCACHE) go test -tags=integration -count=1 ./internal/storage

vet:
	GOCACHE=$(GOCACHE) CGO_ENABLED=0 go vet ./...

html-check:
	GOCACHE=$(GOCACHE) go test -count=1 ./internal/web

image:
	docker build -t calendar-platform:local .
