# Calendar Platform

Calendar Platform is a single-user calendar control plane and MCP server for Google Calendar, Microsoft 365, and Apple iCloud.

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

The authenticated control plane starts at `/app` and adds these pages:

- Dashboard — connection, rule, run, and attention summaries;
- Connections — add, reconnect, verify, discover, and safely remove accounts;
- Sync Rules — create directed routes and run dry runs or manual jobs;
- Runs — sanitized outcomes and mutation counters;
- Settings — installation readiness without displaying secrets.

Google and Microsoft use OAuth 2.0 with expiring single-use state and PKCE. Apple uses an app-specific password. Provider credentials are encrypted with AES-256-GCM before database storage.

Production UI access is designed for Authentik ForwardAuth. With `UI_TRUST_FORWARD_AUTH=true`, `/app` and all connection, rule, run, settings, and OAuth-start routes require the `X-authentik-username` header. The service must not be directly reachable around the trusted reverse proxy: the proxy must discard any client-supplied identity header and set the authenticated value itself. The public landing/policy routes and OAuth callbacks remain outside ForwardAuth; callbacks are protected by their state/PKCE flow. Mutating UI requests additionally require an exact public origin and a same-site CSRF token.

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
| `ALLOW_UNAUTHENTICATED` | `false` | Explicit local-only escape hatch when no API key is configured |
| `ENABLE_V2` | `false` | Exposes typed V2 MCP tools and REST routes |
| `TOKEN_DIR` | `/app/data` | Legacy standalone OAuth token-file directory |

The API fails closed when `API_KEY` is empty unless `ALLOW_UNAUTHENTICATED=true` is explicitly set. During key rotation, set `API_KEY_LEGACY` to the previous key so existing MCP and REST clients continue to work while new clients move to `API_KEY`. Remove `API_KEY_LEGACY` after every client has migrated; it never replaces the mandatory primary key.

### Platform mode

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | empty | Enables platform mode; accepts `postgres://`, `postgresql://`, or `sqlite://` |
| `CALENDAR_ENCRYPTION_KEY` | empty | Base64-encoded 32-byte key for provider credentials |
| `CALENDAR_PUBLIC_URL` | empty | Absolute external UI/OAuth origin, without a trailing slash |
| `UI_TRUST_FORWARD_AUTH` | `false` | Explicitly trusts the Authentik identity header on protected UI routes; enable only behind the documented trusted proxy boundary |
| `UI_ALLOW_UNAUTHENTICATED` | `false` | Explicit local-only UI bypass; rejected unless set when ForwardAuth is disabled |

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

`GOOGLE_REFRESH_TOKEN`, `MS365_REFRESH_TOKEN`, `APPLE_USERNAME`, and `APPLE_APP_PASSWORD` are retained only for standalone compatibility mode.

### Fan-out filtering

| Variable | Purpose |
|---|---|
| `EXCLUDE_CALENDAR_IDS` | Comma-separated canonical or legacy calendar IDs skipped by implicit fan-out |
| `INCLUDE_IMPORTED_CALENDARS` | Includes Google `@import.calendar.google.com` calendars in implicit fan-out |

These settings affect only `get_events` calls without an explicit `calendar_id`. Explicit reads and `list_calendars` still expose the calendar.

## MCP API

MCP uses Streamable HTTP at `/mcp`.

The compatibility catalog is always registered:

| Tool | Purpose |
|---|---|
| `list_calendars` | List calendars across connected accounts |
| `get_events` | Read an explicit calendar or fan out across providers |
| `create_event` | Create a calendar event |
| `update_event` | Partially update an event |
| `delete_event` | Delete an event |

With `ENABLE_V2=true`, the typed lifecycle catalog is also registered:

| Tool | Purpose |
|---|---|
| `get_calendar_capabilities` | Exact operations, fields, scopes, and notification policies |
| `get_events_v2`, `get_event_v2` | Typed recurrence-aware event reads |
| `get_event_instances_v2` | Bounded recurring instances |
| `search_events_v2` | Calendar search or provider fan-out |
| `create_event_v2` | Typed event creation with notification policy |
| `update_event_v2`, `delete_event_v2` | Series, single-instance, and supported following mutations |
| `respond_to_event_v2` | RSVP through Google |
| `move_event_v2` | Move supported Google events between calendars |
| `import_event_v2` | Import an external event into Google using `ical_uid` |

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
make assets            # verify or fetch pinned HTMX 2.0.4
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

The runtime image contains the compiled binary, CA certificates, timezone data, templates, CSS, and checksum-verified HTMX. It has no runtime CDN dependency.

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
