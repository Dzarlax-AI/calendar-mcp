# calendar-mcp

Unified Calendar MCP server (Go + mcp-go). Aggregates Google Calendar, Microsoft 365, and Apple CalDAV.

## Commands

```bash
go build -o server ./cmd/server       # Build
go test -race -count=1 ./...          # Full Go test suite
go vet ./...                          # Lint
API_KEY=test ./server                 # Run locally (add provider env vars as needed)
```

Go version: 1.27.3 (from go.mod)

## Architecture

- `internal/calendar/` — Provider interface, Registry (prefix-based routing), shared types
- `internal/google/` — Google Calendar API v3 via OAuth2
- `internal/microsoft/` — Microsoft Graph REST API via OAuth2
- `internal/apple/` — Apple CalDAV via go-webdav (basic auth)
- `internal/mcpserver/` — MCP server (Streamable HTTP), modern/compat catalogs, API key middleware
- `internal/token/` — File-based OAuth2 token persistence
- `internal/config/` — Env-based config

## Apple CalDAV Notes

- Family Sharing calendars have hash-based paths (`/calendars/<64-char-hex>/`) — different from regular UUID paths
- Apple's CalDAV REPORT (`calendar-query`) is broken for these: returns HTTP 500 + malformed/truncated XML
- `calendar-multiget` REPORT also returns 404 for these calendars
- Fallback implemented in `GetEvents`: PROPFIND Depth:1 to list `.ics` paths → 20 concurrent GETs → filter by date range in code
- This matches how DAVx5 and other clients handle Apple Family Sharing

## MCP Tools

- `/mcp` and `/mcp/` expose one catalog of 12 typed tools. The six reads use local publications and authorization. Writes retain notification, preview, ETag, scope, and write receipt behavior.
- MCP requires platform storage/cipher, the event read model and a primary API key. Workers with the event read model enabled refresh grants and publications. Explicit REST-only and native deployments retain their own contracts.
- Preserve wire schemas and bound event pages to 16 KiB including both SDK result copies. Preserve complete and partial coverage semantics, pagination bindings, and provider-free reads.
- Standalone and retired catalog paths are unavailable for MCP. The `calendar_mcp_legacy_fixture` tag enables historical synthetic harnesses only. Gateway ACLs and routes require separate verification.

Calendar IDs are prefixed: `google:primary`, `microsoft:<id>`, `apple:<path>` (canonical multi-account IDs also remain supported).

### Platform cache and authorization

- Migration 029 adds server-generated `authorization_revision` and role-bound `detail_receipt`. Google Begin fences leases/generations and purges before provider construction; only complete staged discovery opens receipts. Normal OAuth refresh preserves authorization and uses snapshot CAS. Each actual HTTP request captures the token binding under the existing TokenSource mutex.
- Writer tickets must precede provider I/O. Late sync/repair/application responses and stale token errors cannot acquire current authority. Keep connection → sync-state lock order. Keep Google scalar event timestamps in UTC; preserve original payload offsets/timezones. SQLite range UNION arms must stay covering; apply timed precedence before LIMIT outside the date arm.
- SQLite cached pages select at most page_size+1 rows before guarded summary JSON projection in a separate materialized stage; keep cursor filtering before costly master JSON checks and master exclusion before LIMIT. Final authority validation batches every expected tuple in one fresh transaction after Pack, preserving duplicate-ticket semantics and all receipt/revision checks. Do not hold a connection through Pack or HTTP output.
- SQLite page ranges use global timed/date branches with native cursor bounds and covering index eligibility before rowid lookup. Validate all sources in the same read transaction before running the SQLite range SQL, which relies on that validation for calendar and connection status. Timed continuations preflight the numeric timestamp range and keep the legacy-query fallback in that transaction. The master filter may skip JSON parsing only for text with neither the literal `seriesMaster` nor a JSON Unicode escape. Keep master exclusion before LIMIT and payload validation on the selected rows.
- SQLite full cached UI reads use a per-call buffer capped at 8 MiB raw payloads and 8192 rows, closing SQL rows before decode when the result fits. Overflow flushes the buffer and permanently streams the rest of that call. Release raw entries while decoding; do not reuse buffers across calls. PostgreSQL remains streaming. These limits do not bound total result or process memory.
- Legacy Google bootstrap attempts at most one eligible account per worker cycle. Failure/cancellation/crash stays closed until explicit reconnect. This can temporarily remove UI/native cached projection; preserve Apple auth rechecks and Microsoft/Apple behavior.
- Before a separately authorized activation, quiesce all old API/worker/cache writers. Migration version checks cannot stop an already running old writer. Do not mix unfenced old writers with the new reader or roll schema 029 back to an older binary.

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `LISTEN_ADDR` | `:8080` | MCP server listen address |
| `REST_LISTEN_ADDR` | _(empty)_ | Internal REST API (optional) |
| `ENABLE_V2` | `false` | Enable internal REST V2 |
| `EVENT_READ_MODEL_ENABLED` | `false` | Enable the platform read model required by MCP |
| `API_KEY` | _(empty)_ | Auth key for MCP endpoints |
| `NATIVE_APP_TOKEN` | _(empty)_ | Dedicated token for the read-only native app API; requires platform storage |
| `READ_ONLY_TOKEN` | _(empty)_ | Token restricted to cached events and the read-only calendar catalog for HomeDash; requires platform storage |
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

## Calendar listing and Apple discovery
- ListCalendars takes one provider snapshot and uses at most four workers per request. Preserve input order/canonical IDs, copy provider data before prefixing, and return no partial list on provider errors. No global admission contract is added.
- Apple discovery cache is provider-instance scoped with fixed ten-minute TTL measured from successful discovery completion; only principal/home-set is cached, never calendars/events. Waiting callers share discovery success/error and can cancel independently; an initiating caller cancellation aborts that flight.
- Observe original 401/403/404/410 statuses in listing contexts before transport normalization; invalidate discovery, return error, and do not retry the invocation. New provider instances start empty. Event fallback normalization remains unchanged.
- Local synthetic benchmark: go run -tags calendar_mcp_legacy_fixture scripts/measure_calendar_listing.go. Gateway integration: go build -tags calendar_mcp_legacy_fixture -o /tmp/calendar-listing-fixture scripts/measure_calendar_listing.go then python3 scripts/verify_calendar_listing_gateway.py; requires a local /tmp/mcp-gateway-read-batch binary. No real credentials/data.
