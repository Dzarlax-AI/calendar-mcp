# calendar-mcp

Unified Calendar MCP server that aggregates Google Calendar, Microsoft 365, and Apple iCloud (CalDAV) into a single MCP endpoint.

> **Related:** [calendar-sync](https://github.com/dzarlax/calendar-sync) — one-way M365 → Google Calendar sync built on top of this server's REST API.

## Features

- **Google Calendar V2** — recurrence series and instances, search, RSVP, import, move, Meet, attachments, reminders, special event types, ETags, and recoverable this-and-following
- **Microsoft 365** — safe portable CRUD and Teams meetings via Graph REST API (OAuth2), with unsupported V2 fields rejected explicitly
- **Apple iCloud** — safe series-level CRUD and iCalendar recurrence via CalDAV (app-specific password)
- **Unified interface** — five compatibility tools plus typed V2 lifecycle tools
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

The compatibility tools remain available. Set `ENABLE_V2=true` to additionally expose:

| V2 tool | Description |
|---|---|
| `get_calendar_capabilities` | Report the exact operations, fields, scopes, and notification policies supported by a calendar |
| `get_events_v2` / `get_event_v2` | Read typed events with recurrence identity, provider metadata, pagination, and completeness |
| `get_event_instances_v2` | Read bounded occurrences of a recurring Google series |
| `search_events_v2` | Search one calendar or fan out across configured providers |
| `create_event_v2` | Create a typed event; notifications default to `none` |
| `update_event_v2` / `delete_event_v2` | Mutate `series`, `single`, or recoverable Google `following` scope |
| `respond_to_event_v2` | RSVP as the authenticated Google user |
| `move_event_v2` | Move a supported Google event between Google calendars |
| `import_event_v2` | Import an external Google event using `ical_uid` |

Google `following` is deliberately reported as a composite operation. It supports a non-mutating preview, ETag protection, an idempotency marker, compensation, and explicit recovery metadata if both the primary and compensating writes fail. `RDATE`-based series are rejected for this workflow because they cannot yet be split without ambiguity.

`create_event` and `update_event` accept either RFC3339 datetimes (`2026-05-30T13:00:00+02:00`) or all-day date-only boundaries (`2026-05-30` to `2026-05-31`). Date-only boundaries are exclusive on `end`, matching Google Calendar and iCalendar all-day semantics.

## Configuration

All configuration via environment variables:

```bash
# Server
LISTEN_ADDR=:8080
API_KEY=your-api-key
# Explicit local-only opt-in when no API key is configured
ALLOW_UNAUTHENTICATED=false
ENABLE_V2=false
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

# Fan-out filtering (affects get_events without calendar_id only)
EXCLUDE_CALENDAR_IDS=            # comma-separated prefixed IDs to skip
INCLUDE_IMPORTED_CALENDARS=      # set true to include google:*@import.calendar.google.com

# Internal REST API (optional, separate port)
REST_LISTEN_ADDR=
```

Providers are enabled automatically when their credentials are set. You can run with any subset (e.g. Google only).

The server fails closed when `API_KEY` is empty. Set `ALLOW_UNAUTHENTICATED=true` only for an intentionally unauthenticated local instance. Keep `ENABLE_V2=false` during rollout, then enable it after clients have adopted the typed schemas. Legacy Google writes suppress attendee notifications by default. V2 writes default to `notification_policy=none`; providers reject writes whose scheduling side effects cannot be suppressed.

Before any first run against real calendar data, use an empty dedicated test calendar, omit attendees, and verify `notification_policy=none`. The automated test suite uses local fake HTTP/CalDAV endpoints and never writes to real providers.

By default fan-out skips Google ICS subscriptions (typical M365/iCloud mirrors) to avoid duplicate events. Explicit `calendar_id` queries and `list_calendars` are unaffected — downstream consumers like Granola can still reach them.

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

## Apple CalDAV Notes

Apple iCloud CalDAV has quirks for certain calendar types:

- **Family Sharing calendars** have hash-based paths (`/calendars/<64-char-hex>/`) instead of UUID paths
- Apple's server returns HTTP 500 with malformed XML for CalDAV `REPORT` (calendar-query) on these calendars — a known Apple server-side bug
- **Workaround**: the server automatically falls back to `PROPFIND Depth:1` (enumerate `.ics` paths) + 20 concurrent `GET` requests when `REPORT` fails, then filters by date range in code
- This is transparent to the caller — events are returned normally, just with slightly higher latency for large calendars

## Architecture

```
cmd/server/main.go          — entrypoint, provider init, HTTP server
internal/
  config/                    — env-based configuration
  calendar/                  — Provider interface, Registry (prefix routing), types
  application/               — V2 use cases, capabilities, fan-out, recoverable composite operations
  google/                    — Google Calendar API v3 + OAuth2
  microsoft/                 — Microsoft Graph REST API + OAuth2
  apple/                     — CalDAV client (go-webdav) + basic auth
  mcpserver/                 — MCP server (Streamable HTTP), tools, API key middleware
  token/                     — File-based OAuth2 token persistence with auto-refresh
```

## License

MIT
