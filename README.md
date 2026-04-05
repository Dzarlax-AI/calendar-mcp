# calendar-mcp

Unified Calendar MCP server that aggregates Google Calendar, Microsoft 365, and Apple iCloud (CalDAV) into a single MCP endpoint.

## Features

- **Google Calendar** — full CRUD via Calendar API v3 (OAuth2)
- **Microsoft 365** — full CRUD via Graph REST API (OAuth2)
- **Apple iCloud** — full CRUD via CalDAV protocol (app-specific password)
- **Unified interface** — all calendars accessible through 5 MCP tools
- **Provider-prefixed IDs** — `google:primary`, `microsoft:<id>`, `apple:<path>`
- **Concurrent fan-out** — `get_events` without calendar_id queries all providers in parallel

## MCP Tools

| Tool | Description |
|---|---|
| `list_calendars` | List all calendars across all providers |
| `get_events` | Get events by calendar ID and date range (or all calendars) |
| `create_event` | Create event in a specific calendar |
| `update_event` | Partial update of an existing event |
| `delete_event` | Delete an event |

## Configuration

All configuration via environment variables:

```bash
# Server
LISTEN_ADDR=:8080
API_KEY=your-api-key
TOKEN_DIR=/app/data

# Google Calendar (OAuth2 — Desktop app type)
GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=
GOOGLE_REFRESH_TOKEN=

# Microsoft 365 (Azure AD OAuth2)
MS365_CLIENT_ID=
MS365_CLIENT_SECRET=
MS365_TENANT_ID=common
MS365_REFRESH_TOKEN=

# Apple iCloud (CalDAV)
APPLE_USERNAME=
APPLE_APP_PASSWORD=
APPLE_CALDAV_URL=https://caldav.icloud.com/
```

Providers are enabled automatically when their credentials are set. You can run with any subset (e.g. Google only).

## Getting OAuth Tokens

### Google

1. Create a **Desktop** OAuth client in [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
2. Open the consent URL:
   ```
   https://accounts.google.com/o/oauth2/v2/auth?client_id=YOUR_CLIENT_ID&redirect_uri=http://localhost&response_type=code&scope=https://www.googleapis.com/auth/calendar&access_type=offline&prompt=consent
   ```
3. Copy the `code` from the redirect URL
4. Exchange for refresh token:
   ```bash
   curl -s -X POST https://oauth2.googleapis.com/token \
     -d "code=AUTH_CODE" \
     -d "client_id=YOUR_CLIENT_ID" \
     -d "client_secret=YOUR_SECRET" \
     -d "redirect_uri=http://localhost" \
     -d "grant_type=authorization_code"
   ```

### Microsoft 365

1. Register an app in [Azure AD](https://portal.azure.com/#blade/Microsoft_AAD_RegisteredApps)
2. Add `Calendars.ReadWrite` and `offline_access` permissions
3. Complete OAuth2 consent flow to obtain a refresh token

### Apple

Generate an [app-specific password](https://appleid.apple.com/) — no OAuth needed.

## Build & Run

```bash
go build -o server ./cmd/server
API_KEY=test GOOGLE_CLIENT_ID=... ./server
```

## Docker

```bash
docker build -t calendar-mcp .
docker run -e API_KEY=... -e GOOGLE_CLIENT_ID=... -p 8080:8080 -v ./data:/app/data calendar-mcp
```

## Deploy

Docker image built via GitHub Actions: `ghcr.io/dzarlax-ai/calendar-mcp:latest`

MCP endpoint: `https://mcp.dzarlax.dev/calendar` (Traefik path rewrite `/calendar` → `/mcp`)

## Architecture

```
cmd/server/main.go          — entrypoint, provider init, HTTP server
internal/
  config/                    — env-based configuration
  calendar/                  — Provider interface, Registry (prefix routing), types
  google/                    — Google Calendar API v3 + OAuth2
  microsoft/                 — Microsoft Graph REST API + OAuth2
  apple/                     — CalDAV client (go-webdav) + basic auth
  mcpserver/                 — MCP server (Streamable HTTP), tools, API key middleware
  token/                     — File-based OAuth2 token persistence with auto-refresh
```

## License

MIT
