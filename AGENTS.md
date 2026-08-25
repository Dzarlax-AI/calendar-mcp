# calendar-mcp

Unified Calendar MCP server (Go + mcp-go). Aggregates Google Calendar, Microsoft 365, and Apple CalDAV.

## Commands

```bash
go build -o server ./cmd/server       # Build
go test -race -count=1 ./...          # Tests (no test files yet, but CI runs this)
go vet ./...                          # Lint
API_KEY=test ./server                 # Run locally (add provider env vars as needed)
```

Go version: 1.25 (from go.mod)

## Architecture

- `internal/calendar/` — Provider interface, Registry (prefix-based routing), shared types
- `internal/google/` — Google Calendar API v3 via OAuth2
- `internal/microsoft/` — Microsoft Graph REST API via OAuth2
- `internal/apple/` — Apple CalDAV via go-webdav (basic auth)
- `internal/mcpserver/` — MCP server (Streamable HTTP), 5 tools, API key middleware
- `internal/token/` — File-based OAuth2 token persistence
- `internal/config/` — Env-based config

## Apple CalDAV Notes

- Family Sharing calendars have hash-based paths (`/calendars/<64-char-hex>/`) — different from regular UUID paths
- Apple's CalDAV REPORT (`calendar-query`) is broken for these: returns HTTP 500 + malformed/truncated XML
- `calendar-multiget` REPORT also returns 404 for these calendars
- Fallback implemented in `GetEvents`: PROPFIND Depth:1 to list `.ics` paths → 20 concurrent GETs → filter by date range in code
- This matches how DAVx5 and other clients handle Apple Family Sharing

## MCP Tools

- `list_calendars` — all calendars from all providers
- `get_events` — events by calendar_id + date range (or all)
- `create_event` — create event in specific calendar
- `update_event` — partial update
- `delete_event` — delete event

Calendar IDs are prefixed: `google:primary`, `microsoft:<id>`, `apple:<path>`

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `LISTEN_ADDR` | `:8080` | MCP server listen address |
| `REST_LISTEN_ADDR` | _(empty)_ | Internal REST API (optional) |
| `API_KEY` | _(empty)_ | Auth key for MCP endpoints |
| `TOKEN_DIR` | `/app/data` | OAuth2 token file storage |
| `GOOGLE_CLIENT_ID` | | Google OAuth2 client ID |
| `GOOGLE_CLIENT_SECRET` | | Google OAuth2 client secret |
| `GOOGLE_REFRESH_TOKEN` | | Pre-obtained Google refresh token |
| `MS365_CLIENT_ID` | | Microsoft OAuth2 client ID |
| `MS365_CLIENT_SECRET` | | Microsoft OAuth2 client secret |
| `MS365_TENANT_ID` | | Azure tenant ID |
| `MS365_REFRESH_TOKEN` | | Pre-obtained Microsoft refresh token |
| `APPLE_USERNAME` | | iCloud username |
| `APPLE_APP_PASSWORD` | | Apple app-specific password |
| `APPLE_CALDAV_URL` | `https://caldav.icloud.com` | CalDAV endpoint |
| `EXCLUDE_CALENDAR_IDS` | _(empty)_ | Comma-separated prefixed calendar IDs skipped in fan-out `get_events` (e.g. `google:xyz@import.calendar.google.com`). Explicit `calendar_id` queries still work. |
| `INCLUDE_IMPORTED_CALENDARS` | `false` | By default fan-out auto-skips Google ICS subscriptions (`google:*@import.calendar.google.com`) to avoid duplicates from M365/iCloud mirrors. Set `true` to include them. |

Providers initialize only if their credentials are set. No credentials = provider skipped silently.

## OAuth2 Token Flow

- Google: desktop flow with `http://localhost` redirect, token saved to `{TOKEN_DIR}/google_token.json`
- Microsoft: tenant-specific flow, token saved to `{TOKEN_DIR}/microsoft_token.json`
- Apple: no OAuth2, uses HTTP Basic Auth with app-specific password
- Tokens auto-refresh and persist via `persistingTokenSource` (file permissions `0600`)

## Deploy

Docker image built via GitHub Actions → `ghcr.io/dzarlax-ai/calendar-mcp:latest`
Deploy config: `personal_ai_stack/deploy/calendar-mcp/`
Route: `mcp.dzarlax.dev/calendar` (Traefik path rewrite → /mcp)
Health check: `GET /health`
