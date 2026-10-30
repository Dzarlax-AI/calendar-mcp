# Calendar platform

Calendar platform is a single-user calendar control plane and MCP server for Google Calendar, Microsoft 365, and Apple iCloud.

One Go repository and one container image provide:

- MCP and REST calendar APIs;
- a web UI for provider connections and sync rules;
- encrypted multi-account credential storage;
- a separate synchronization worker;
- one-way calendar copies across every provider pairing;
- PostgreSQL for production and SQLite for small self-hosted installations.

The image has two long-running commands:

- `calendar serve` runs migrations, MCP, optional REST, OAuth callbacks, and the web UI;
- `calendar worker` schedules and executes synchronization jobs.

The former `calendar-sync` repository is migration history and rollback material. New synchronization development belongs here.

## Status and safety model

The platform is intentionally conservative around calendar writes:

- every sync rule is one-way;
- new rules start paused;
- a successful read-only dry run is required before enablement;
- attendees are never copied;
- provider notifications and invitations are fixed to `none`;
- direct and transitive rule cycles are rejected;
- unsupported recurrence is a blocker, never silently flattened;
- absence from a bounded sync window is not treated as deletion evidence;
- only one job for a rule may run at a time;
- abandoned jobs are recoverable after a 15-minute worker lease expires.

Before using real data, connect dedicated empty calendars and verify the complete dry-run and write path. Automated tests use fake provider endpoints and never write to real calendars.

## Architecture

```text
                         one calendar image
                    ┌────────────────────────┐
MCP clients ───────▶│ calendar serve         │
REST clients ──────▶│  /mcp                  │
Authentik ─────────▶│  web UI + OAuth        │
                    │  provider registry     │
                    └───────────┬────────────┘
                                │
                         PostgreSQL / SQLite
                                │
                    ┌───────────▼────────────┐
                    │ calendar worker        │
                    │ scheduler + sync engine│
                    └───────────┬────────────┘
                                │
                    Google / Microsoft / Apple
```

`serve` owns schema migration. `worker` checks the schema version and stays unavailable when it does not match the binary. Both processes use the same persisted provider connections and account-scoped provider registry, so calendars connected in the UI are immediately available to MCP and REST.

## Provider and synchronization support

Multiple accounts can be connected for every provider. A calendar has a canonical account-scoped ID:

```text
google:<connection-id>:<provider-calendar-id>
microsoft:<connection-id>:<provider-calendar-id>
apple:<connection-id>:<provider-calendar-id>
```

The legacy `provider:calendar-id` form remains accepted only when it resolves to exactly one connected account. Ambiguous aliases are rejected.

Any readable calendar can be a source and any writable calendar can be a target:

| Source | Google target | Microsoft target | Apple target |
|---|---:|---:|---:|
| Google | Yes | Yes | Yes |
| Microsoft | Yes | Yes | Yes |
| Apple | Yes | Yes | Yes |

This includes copies between two calendars owned by the same provider or account. It is not bidirectional conflict resolution: configure directed rules, and keep the overall rule graph acyclic.

### Recurrence

The sync engine reads bounded series and instance views and preserves:

- series masters;
- ordinary occurrences;
- modified exceptions;
- cancelled occurrences;
- all-day boundaries;
- original occurrence identity and time zone data.

Google and Apple can express broader iCalendar recurrence than Microsoft Graph. A Google or Apple series that cannot be represented exactly by Graph fails preflight with `recurrence_unsupported`. The rule is not approximated or flattened.

Google targets receive private rule/source markers. Before a create, the engine checks those markers so a target created before a timeout or local mapping failure can be recovered on the next run. Microsoft and Apple currently rely on persisted mappings and do not yet provide equivalent provider-side marker recovery for an ambiguous remote create.

## Web control plane

Platform mode exposes a public product landing page at `/`, plus the repository-maintained [Privacy Policy](internal/web/legal/privacy.md) at `/privacy` and [Terms of Service](internal/web/legal/terms.md) at `/terms`. These routes contain no connection or calendar state. They can be routed publicly for OAuth application verification while the control plane stays authenticated.

The authenticated React application starts at `/app`. Its primary screen is a unified calendar with day, week, month, and list views, connected-calendar filters, event details, and capability-aware create/edit/reschedule/delete actions. The remaining control-plane pages are:

- Connections — add, reconnect, verify, discover, and safely remove accounts;
- Sync Rules — create directed routes and run dry runs or manual jobs;
- Runs — sanitized outcomes and mutation counters;
- Settings — installation readiness plus deliberate, short-lived MCP access-key reveal for a single-user installation.

Google and Microsoft use OAuth 2.0 with expiring single-use state and PKCE. Apple uses an app-specific password. Provider credentials are encrypted with AES-256-GCM before database storage.

Production UI access is designed for Authentik ForwardAuth. With `UI_TRUST_FORWARD_AUTH=true`, `/app` and all connection, rule, run, settings, and OAuth-start routes require the `X-authentik-username` header. The service must not be directly reachable around the trusted reverse proxy: the proxy must discard any client-supplied identity header and set the authenticated value itself. The public landing/policy routes and OAuth callbacks remain outside ForwardAuth; callbacks are protected by their state/PKCE flow. Mutating UI requests additionally require an exact public origin and a same-site CSRF token.

The browser uses a same-origin JSON API under `/api/ui/`. This is intentionally narrower than MCP and REST: bootstrap and event responses omit credentials, attendees, reminders, conferencing, attachments, and API keys; mutations force provider notifications to `none`. Event identity is passed through query parameters on `/api/ui/event` so Apple CalDAV identifiers containing `/` remain intact. The browser API calls the shared typed application service directly and does not expose the API-key-protected REST listener.

### Frontend development

The authenticated client lives in `frontend/` and is built before the Go binary is embedded:

```bash
npm --prefix frontend ci
npm --prefix frontend run typecheck
npm --prefix frontend test
npm --prefix frontend run build
make build
```

`make build`, the Dockerfile, and CI all stage `frontend/dist/` into the Go embed directory before compiling. Generated bundles are ignored; only the source-controlled placeholder is retained. Automated and visual QA must use fake providers or synthetic data. Before any later live mutation, use a dedicated empty calendar and separately confirm invitation and notification suppression.

### Local Calendar mock workbench

For UI polishing without Authentik, provider credentials, or real calendar data:

```bash
npm --prefix frontend ci
npm --prefix frontend run dev:mocks
```

Open `http://localhost:5173/app`. This serves the frontend workbench with in-memory synthetic `/api/ui` responses only; create, edit, delete, and refresh stay local and reset when Vite restarts. The regular `npm --prefix frontend run dev` command is unchanged and does not install mock routes.

The Settings page shows the MCP endpoint and can reveal the primary `API_KEY` only after an explicit authenticated, same-origin, CSRF-protected action. The key is absent from the initial HTML and is removed from the page after 30 seconds, when hidden, or when the page exits. Copying uses the browser Clipboard API; the application does not automatically clear the OS clipboard because doing so could overwrite newer clipboard contents. `API_KEY_LEGACY` is never revealed and appears only as a configured/not-configured status. `NATIVE_APP_TOKEN` is never shown in the web UI.

This convenience assumes a single-user Authentik boundary. Every identity accepted by that boundary can retrieve the primary master key, and a compromised browser session can do the same. Before enabling multi-user UI access, replace master-key reveal with independently revocable per-client credentials.

## Quick start with SQLite

Requirements:

- Docker with Compose, or Go 1.25+ for a source build;
- a random 32-byte encryption key encoded as base64;
- OAuth application credentials for providers you want to connect.

Create `.env` from `.env.example`, replace every placeholder, then run:

```bash
./scripts/compose.sh sqlite up -d
```

Set `CALENDAR_IMAGE` to a reviewed immutable `image@sha256:...` reference. The wrapper renders the Compose model, validates every resolved service image, and rejects missing, tagged, or malformed references before starting any service. The example starts `calendar-serve` and `calendar-worker` from that same image and shares `/app/data` between them. It explicitly enables the unauthenticated UI bypass and binds the port to `127.0.0.1`, so the landing page is available at `http://localhost:8080/` and the control plane at `http://localhost:8080/app` only from the same machine. Do not expose this example through a public interface.

The SQLite deployment supports one worker only. Use PostgreSQL for production or multiple service replicas.

## PostgreSQL deployment

The production-oriented example uses a configurable PostgreSQL image and keeps the UI in ForwardAuth mode. PostgreSQL 17 is the tested and recommended major version; set `POSTGRES_IMAGE` to a reviewed PostgreSQL 17 digest. Set `CALENDAR_IMAGE` to a reviewed calendar-service digest and `CALENDAR_POSTGRES_PASSWORD` to a random URL-safe value before rendering Compose:

```bash
./scripts/compose.sh postgres up -d
```

The wrapper requires every resolved service image to use the exact `image@sha256:<64-hex-digest>` form before any of the three services starts. Put a configured Authentik reverse proxy in front of the loopback-bound service before using the UI. The proxy must strip any client-supplied `X-authentik-username` header and set the authenticated identity itself.

The default `sslmode=disable` connection is only for the bundled PostgreSQL container on the private Compose network. For an existing managed or shared-network PostgreSQL instance, set `CALENDAR_DATABASE_URL` to a certificate-verified connection such as `postgres://calendar:password@db.example.com/calendar?sslmode=verify-full&sslrootcert=/run/secrets/postgres-ca.pem`, mount the CA certificate, and use a dedicated role/schema. Do not use the bundled non-TLS default outside its local Compose network.

Startup order matters:

1. PostgreSQL becomes healthy.
2. `calendar serve` applies or validates the schema migration.
3. `calendar worker` validates the same schema version and exposes readiness.

Health endpoints:

| Process | Default endpoint |
|---|---|
| `calendar serve` | `http://127.0.0.1:8080/health` |
| `calendar worker` | `http://127.0.0.1:8082/health` |
| optional internal REST | `${REST_LISTEN_ADDR}/health` |

## Configuration

### Core server

| Variable | Default | Purpose |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | MCP, UI, OAuth callback, and serve health listener |
| `WORKER_HEALTH_ADDR` | `127.0.0.1:8082` | Worker readiness listener |
| `REST_LISTEN_ADDR` | empty | Optional separate internal REST listener |
| `API_KEY` | empty | Bearer token or `X-API-Key` accepted by MCP and REST |
| `API_KEY_LEGACY` | empty | Optional previous key accepted temporarily by MCP and REST during rotation |
| `NATIVE_APP_TOKEN` | empty | Dedicated Bearer token for the read-only native app API; requires `DATABASE_URL` and is never revealed in web Settings |
| `READ_ONLY_TOKEN` | empty | Dedicated Bearer token for `GET /api/native/v1/cached-events` and `GET /api/native/v1/calendars`; requires `DATABASE_URL` and has no native write access |
| `NATIVE_APP_WRITES_ENABLED` | `false` | Explicitly permits event writes for the native API token; leave disabled to retain the read-only contract |
| `NATIVE_APP_TRUSTED_PROXY` | `false` | Explicitly confirms that a public native API is reachable only through a trusted HTTPS reverse proxy |
| `ALLOW_UNAUTHENTICATED` | `false` | Explicit local-only escape hatch when no API key is configured |
| `ENABLE_V2` | `false` | Enables typed lifecycle MCP tools and REST V2 routes; required for the modern MCP catalog |
| `MCP_CATALOG_MODE` | `compat` | Catalog at `/mcp`: `compat` or `modern`; modern requires `ENABLE_V2=true` |
| `TOKEN_DIR` | `/app/data` | Legacy standalone OAuth token-file directory |

The API fails closed when `API_KEY`, `NATIVE_APP_TOKEN`, and `READ_ONLY_TOKEN` are all empty unless `ALLOW_UNAUTHENTICATED=true` is explicitly set. During key rotation, set `API_KEY_LEGACY` to the previous key so existing MCP and REST clients continue to work while new clients move to `API_KEY`. Remove `API_KEY_LEGACY` after every client has migrated; it never replaces the mandatory primary key.

### Platform mode

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | empty | Enables platform mode; accepts `postgres://`, `postgresql://`, or `sqlite://` |
| `CALENDAR_ENCRYPTION_KEY` | empty | Base64-encoded 32-byte key for provider credentials |
| `CALENDAR_PUBLIC_URL` | empty | Absolute external UI/OAuth origin, without a trailing slash |
| `UI_TRUST_FORWARD_AUTH` | `false` | Explicitly trusts the Authentik identity header on protected UI routes; enable only behind the documented trusted proxy boundary |
| `UI_ALLOW_UNAUTHENTICATED` | `false` | Explicit local-only UI bypass; rejected unless set when ForwardAuth is disabled |

### Native app API

Set a separate `NATIVE_APP_TOKEN` to enable the read-only native API in platform mode. It is intended for the Swift client, is distinct from the MCP key, and does not require Authentik or `UI_ALLOW_UNAUTHENTICATED`. A public native endpoint requires both an HTTPS `CALENDAR_PUBLIC_URL` and `NATIVE_APP_TRUSTED_PROXY=true`; the proxy must be the only client-facing route to the service. Localhost-only development may use HTTP without the proxy setting. Event writes are disabled by default; set `NATIVE_APP_WRITES_ENABLED=true` only after deliberately granting this token write authority.

```bash
NATIVE_APP_TOKEN="$(openssl rand -base64 32)" \
CALENDAR_ENCRYPTION_KEY="$(openssl rand -base64 32)" \
DATABASE_URL="sqlite:///app/data/calendar.db" \
CALENDAR_PUBLIC_URL="https://calendar.example.com" \
NATIVE_APP_TRUSTED_PROXY=true \
./calendar serve
```

Persist both generated values in deployment secrets before starting the service: changing `CALENDAR_ENCRYPTION_KEY` makes existing provider credentials unreadable, and changing `NATIVE_APP_TOKEN` disconnects configured app clients. Requests must send `Authorization: Bearer <NATIVE_APP_TOKEN>`. The available routes are `GET /api/native/v1/bootstrap` and `GET /api/native/v1/events`. With the default `NATIVE_APP_WRITES_ENABLED=false`, no event-mutation routes are exposed. When the explicit opt-in is enabled, `POST /api/native/v1/events` and `PATCH /api/native/v1/events/{calendar_id}/{event_id}` are available for ordinary events; provider notifications remain forced off. The token is never returned by the Settings page or any API response.

`READ_ONLY_TOKEN` is independent from `NATIVE_APP_TOKEN` and enables only `GET /api/native/v1/cached-events` and `GET /api/native/v1/calendars`. Cached events read the local projection exclusively and return `503` when the event read model is unavailable. The calendar catalog reads stored readable calendars, sorted by name, and always reports them as read-only; it returns `503` with `catalog_too_large` if more than 2000 are readable. Neither route discovers providers. The token never authorizes `/bootstrap`, `/events`, or any write route.

Examples:

```text
sqlite:///app/data/calendar.db
postgres://calendar:password@postgres:5432/calendar?sslmode=disable
postgres://calendar:password@db.example.com/calendar?sslmode=verify-full&sslrootcert=/run/secrets/postgres-ca.pem
```

Back up the encryption key separately from the database. Losing it requires reconnecting every provider account.

### Provider applications

| Variable | Purpose |
|---|---|
| `GOOGLE_CLIENT_ID` | Google OAuth application client ID |
| `GOOGLE_CLIENT_SECRET` | Google OAuth application client secret |
| `MS365_CLIENT_ID` | Microsoft Entra application client ID |
| `MS365_CLIENT_SECRET` | Microsoft Entra application client secret |
| `MS365_TENANT_ID` | Tenant ID or `common` |
| `APPLE_CALDAV_URL` | Apple CalDAV root; defaults to `https://caldav.icloud.com` |

Apple username and app-specific password are entered through the UI in platform mode.

Google OAuth requests only these application scopes:

- `https://www.googleapis.com/auth/calendar.calendarlist.readonly` to discover calendars available to the connected account;
- `https://www.googleapis.com/auth/calendar.events` to read and mutate events where that account already has access.

The application does not request the broad `https://www.googleapis.com/auth/calendar` scope or calendar ACL, settings, or calendar-management scopes. After narrowing an existing Google OAuth application, revoke its previous grant and reconnect the account; changing the requested scope list does not reduce permissions already carried by an issued refresh token.

`GOOGLE_REFRESH_TOKEN`, `MS365_REFRESH_TOKEN`, `APPLE_USERNAME`, and `APPLE_APP_PASSWORD` are retained only for standalone compatibility mode.

### Fan-out filtering

| Variable | Purpose |
|---|---|
| `EXCLUDE_CALENDAR_IDS` | Comma-separated canonical or legacy calendar IDs skipped by implicit fan-out |
| `INCLUDE_IMPORTED_CALENDARS` | Includes Google `@import.calendar.google.com` calendars in implicit fan-out |

These settings affect only `get_events` calls without an explicit `calendar_id`. Explicit reads and `list_calendars` still expose the calendar.

## MCP API

MCP uses Streamable HTTP. Each connection selects one catalog:

| Service path (with or without trailing slash) | Catalog |
|---|---|
| `/mcp` | Selected by `MCP_CATALOG_MODE`; defaults to `compat` |
| `/mcp/modern` | One modern tool set, requires `ENABLE_V2=true` |
| `/mcp/compat` | Original catalog, including the original V2 names when enabled |

Set `MCP_CATALOG_MODE=modern` only when clients using `/mcp` have migrated to the modern schemas; it requires `ENABLE_V2=true`. An unset or empty mode preserves compatibility. Invalid modes fail startup. With V2 disabled, `/mcp/modern` returns 404 after authentication. Unknown MCP subpaths do not select a catalog. All paths use the existing API-key middleware and request body limit. These are service paths; check reverse-proxy rewrites before using them as external URLs. The UI connection link remains `/mcp` and follows its configured mode. REST and native API contracts are unchanged.

### Modern catalog

Use `/mcp/modern` for new connections during migration. It exposes exactly 12 tools and no `_v2` aliases:

| Tool | Purpose |
|---|---|
| `list_calendars` | List calendars across connected accounts |
| `get_calendar_capabilities` | Exact operations, fields, scopes, and notification policies |
| `get_events`, `get_event` | Typed recurrence-aware event reads |
| `get_event_instances` | Bounded recurring instances |
| `search_events` | Calendar search or provider fan-out; Apple requires both `start` and `end` |
| `create_event` | Typed event creation with notification policy |
| `update_event`, `delete_event` | Series, single-instance, and supported following mutations |
| `respond_to_event` | RSVP through Google |
| `move_event` | Move supported Google events between calendars |
| `import_event` | Import an external event into Google using `ical_uid` |

These are the existing typed V2 contracts under stable tool names. The application handlers, input schemas, structured/text outputs, typed errors, notification policies, and provider capabilities are unchanged. `list_calendars` uses the same handler in both catalogs.

Example arguments for modern `get_events`:

```json
{
  "calendar_id": "google:primary",
  "start": "2026-10-24T00:00:00+02:00",
  "end": "2026-10-25T00:00:00+02:00",
  "view": "expanded",
  "max_results": 50
}
```

Read `items`, `complete`, `sources`, and `next_page_token` from the structured result. Follow continuation tokens with the same query parameters; a partial empty page does not establish that no events exist. The result is a page object, whereas compatibility `get_events` returns a legacy list.

Example arguments for modern `create_event` on an isolated test calendar (a write operation):

```json
{
  "calendar_id": "google:isolated-test-calendar",
  "event": {
    "title": "Catalog migration check",
    "start": {"date": "2026-10-24"},
    "end": {"date": "2026-10-25"}
  },
  "notification_policy": "none"
}
```

Check capabilities before mutations. Preserve explicit scopes, ETags, operation IDs, and preview support when adapting update/delete clients. Test real writes only after verifying isolated data and disabled invitations/notifications.

### Compatibility catalog and migration

The compatibility catalog preserves the original five tools: `list_calendars`, `get_events`, `create_event`, `update_event`, and `delete_event`. With `ENABLE_V2=true` it also preserves `get_calendar_capabilities`, `get_events_v2`, `get_event_v2`, `get_event_instances_v2`, `search_events_v2`, `create_event_v2`, `update_event_v2`, `delete_event_v2`, `respond_to_event_v2`, `move_event_v2`, and `import_event_v2`. Existing names, schemas, outputs, and behavior remain available on `/mcp/compat` and on the default `/mcp` in compat mode.

1. Update the service with `MCP_CATALOG_MODE=compat` (the default); current `/mcp` clients keep their contracts.
2. Inventory direct and Gateway consumers, tool names, expected schemas, and permissions before migrating them. Keep one calendar connection per assistant to avoid exposing both catalogs again.
3. Move a selected client to `/mcp/modern`, updating its argument and result schemas. Legacy `get_events` and modern `get_events` have different contracts: changing the URL alone is insufficient. Existing V2 clients remove suffixes and keep their typed schemas.
4. For Gateway consumers, create a separate modern service identity and explicitly map authorized operations. Do not reuse legacy schema caches or infer permission grants from identical tool names. Reconnect and refresh discovery through the client's normal procedure; verify initialize, tools/list, pagination, and completeness with safe reads.
5. Pin remaining compatibility clients to `/mcp/compat`. Only after the inventory and verification are complete, explicitly switch the main endpoint with `MCP_CATALOG_MODE=modern`. No automatic cutover or compatibility removal occurs.

Deployment, proxy/Gateway/ACL changes, and client reconfiguration require their own authorization. To roll back a client, restore its previous compatibility connection and schemas. Before rolling back the binary, move modern clients to a supported compatibility contract: older binaries do not serve the explicit paths. Changing the default mode can affect clients already using `/mcp` as modern; explicit catalog paths remain stable across mode changes.

Apple search expands recurring events in the requested time window and matches a case-insensitive substring in the title, description, or location. It requires both `start` and `end`, supports `show_deleted`, and paginates with `max_results` and `page_token`. Only the `default` event type is available. Continuation tokens belong to the same calendar, query, bounds, deletion policy, and event-type filter; pagination does not provide a snapshot across external edits. Malformed calendar objects fail search rather than producing a misleading complete result. Searching without a time window leaves the Apple source incomplete with `unsupported_capability` and no retry recommendation. Provider-wide failures remain visible in `sources`; an empty partial result does not prove that there are no matching events.

Google `following` is a recoverable composite workflow, not an atomic provider operation. It uses preview, ETag protection, operation markers, compensation, and explicit partial-failure metadata. `RDATE`-based series are rejected because they cannot currently be split unambiguously.

## REST API

Set `REST_LISTEN_ADDR` to expose the compatibility REST API on a separate listener. It uses the same API-key middleware and provider registry as MCP.

Compatibility routes:

```text
GET    /api/calendars
GET    /api/events
POST   /api/events
PATCH  /api/events
DELETE /api/events
```

When `ENABLE_V2=true`, typed routes are also available under `/api/v2`, including capabilities, event lifecycle, instances, search, response, move, and import.

Keep this listener internal unless an external client explicitly requires it.

## Standalone compatibility mode

When `DATABASE_URL` is empty, `calendar serve` runs the original MCP/REST service without the web control plane or worker. Configure providers through environment variables and legacy token files:

```bash
API_KEY=change-me \
GOOGLE_CLIENT_ID=... \
GOOGLE_CLIENT_SECRET=... \
GOOGLE_REFRESH_TOKEN=... \
./calendar serve
```

At least one provider must be configured in this mode. The worker requires platform storage and will refuse to start without `DATABASE_URL`.

## Sync rule lifecycle

1. Connect and verify provider accounts.
2. Confirm discovered calendar read/write capabilities.
3. Create a source-to-target rule.
4. Set independent `lookback_days` and `lookahead_days` values.
5. Review the automatically queued dry run.
6. Enable only after the dry run succeeds.
7. Monitor scheduled or manual runs and sanitized errors.

Allowed intervals are 10, 30, or 60 minutes. Lookback is 0–365 days; lookahead is 1–365 days.

## Legacy sync import

Preview is read-only and does not require a database:

```bash
./calendar import-legacy \
  --state-file /data/sync_state.json \
  --source microsoft:SOURCE_ID \
  --target google:TARGET_ID \
  --preview
```

After the source and target calendars are connected and discovered, omit `--preview` and supply `DATABASE_URL`. The import creates one paused rule and legacy-marked mappings atomically.

The import does not call providers, mutate events, revoke OAuth grants, delete token files, or stop the old worker. A legacy calendar alias must resolve to exactly one account.

## Build and development

```bash
make build             # build ./calendar
make test              # full race-enabled test suite
make vet               # Go static analysis
make html-check        # embedded UI tests
make image             # local calendar-platform:local image
```

PostgreSQL integration tests require a disposable database:

```bash
TEST_POSTGRES_URL='postgres://calendar:password@127.0.0.1:5432/calendar?sslmode=disable' \
make test-integration
```

The runtime image contains the compiled binary, CA certificates, timezone data, templates, CSS, and the first-party action-progress JavaScript. It has no runtime CDN dependency.

## Repository layout

```text
cmd/calendar/          serve, worker, and import-legacy CLI
cmd/server/            compatibility entry point for calendar serve
internal/application/  typed lifecycle use cases
internal/calendar/     provider contracts, routing, and shared types
internal/connections/  encrypted multi-account connection lifecycle
internal/credentials/  AES-256-GCM envelope
internal/google/       Google Calendar API and OAuth adapter
internal/microsoft/    Microsoft Graph and OAuth adapter
internal/apple/        Apple CalDAV adapter and bounded recurrence expansion
internal/oauthflow/    state, PKCE, exchange, and reconnect intent
internal/providers/    account-scoped provider factory
internal/storage/      PostgreSQL/SQLite schema and operations
internal/syncengine/   one-way reconciliation and recurrence handling
internal/web/          embedded UI templates and assets
```

## Production rollout checklist

- build and review an immutable image digest;
- back up the existing calendar environment, tokens, and legacy sync state;
- create the dedicated PostgreSQL role/schema and back up the encryption key;
- place the UI behind Authentik and keep OAuth callback routes reachable;
- validate MCP, REST, UI, serve health, and worker readiness;
- connect dedicated empty calendars first;
- verify standalone, all-day, recurring, moved, and cancelled occurrences;
- verify `notification_policy=none` and that attendees are absent;
- run legacy import preview before any database write;
- never run the legacy and new synchronization workers against the same route concurrently.

No deployment or provider grant change is performed by building this repository.

## License

MIT

## Calendar listing performance

`list_calendars` reads connected accounts concurrently with at most four provider
calls per aggregate request. This is a per-request limit, not a global service
admission limit. Results retain provider/calendar order and canonical IDs; a
provider error returns an error rather than an incomplete successful list.

Apple providers cache successful principal/home-set discovery for ten minutes
from refresh completion. Each listing still fetches the live calendar list, so
new calendars and changed names do not wait for a list-cache TTL. Cache state is
private to a provider instance and resets on provider replacement/reconnection.
Concurrent discovery callers share one result, including errors; errors and
canceled discovery are not cached. Canceling a waiting caller does not cancel
the initiating caller. Canceling the initiating caller aborts its shared flight.
Raw HTTP 401/403/404/410 invalidate discovery even if Apple's existing transport
normalizes a multistatus response to 207. The failed invocation is not retried;
a later invocation can rediscover. Other event transport behavior is unchanged.

Synthetic local comparison (no real credentials or providers):

```bash
go run scripts/measure_calendar_listing.go
go build -o /tmp/calendar-listing-fixture scripts/measure_calendar_listing.go
python3 scripts/verify_calendar_listing_gateway.py
```

The integration script additionally needs the local gateway test binary at
`/tmp/mcp-gateway-read-batch`; it creates disposable synthetic configuration and
terminates its fixture processes. Production deployment is a separate action.
