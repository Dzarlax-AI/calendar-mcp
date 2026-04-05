# calendar-mcp

Unified Calendar MCP server (Go + mcp-go). Aggregates Google Calendar, Microsoft 365, and Apple CalDAV.

## Build & Run

```bash
go build ./cmd/server/
API_KEY=test GOOGLE_CLIENT_ID=... ./server
```

## Architecture

- `internal/calendar/` — Provider interface, Registry (prefix-based routing), shared types
- `internal/google/` — Google Calendar API v3 via OAuth2
- `internal/microsoft/` — Microsoft Graph REST API via OAuth2
- `internal/apple/` — Apple CalDAV via go-webdav (basic auth)
- `internal/mcpserver/` — MCP server (Streamable HTTP), 5 tools, API key middleware
- `internal/token/` — File-based OAuth2 token persistence
- `internal/config/` — Env-based config

## MCP Tools

- `list_calendars` — all calendars from all providers
- `get_events` — events by calendar_id + date range (or all)
- `create_event` — create event in specific calendar
- `update_event` — partial update
- `delete_event` — delete event

Calendar IDs are prefixed: `google:primary`, `microsoft:<id>`, `apple:<path>`

## Deploy

Docker image built via GitHub Actions → ghcr.io/dzarlax/calendar-mcp
Deploy config: `personal_ai_stack/deploy/calendar-mcp/`
Route: `mcp.dzarlax.dev/calendar` (Traefik path rewrite → /mcp)
