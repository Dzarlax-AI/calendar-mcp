.PHONY: build frontend-install frontend-test frontend-build test test-integration vet html-check image

GOCACHE ?= /tmp/calendar-mcp-go-build

build:
	$(MAKE) frontend-build
	GOCACHE=$(GOCACHE) CGO_ENABLED=0 go build -o calendar ./cmd/calendar

frontend-install:
	npm --prefix frontend ci

frontend-test: frontend-install
	npm --prefix frontend run typecheck
	npm --prefix frontend test

frontend-build: frontend-install
	npm --prefix frontend run build
	mkdir -p internal/web/spa/dist
	find internal/web/spa/dist -depth -mindepth 1 ! -name placeholder.txt -delete
	cp -R frontend/dist/. internal/web/spa/dist/

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
