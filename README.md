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

`serve` owns schema migration. `worker` checks the schema version and stays unavailable when it does not match the binary. Both processes use the same persisted provider connections and account-scoped provider registry. MCP reads use the grants and event publications produced by the worker. REST uses its configured provider paths.

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

For cache-backed copies to Google, Google assigns the event ID and iCalUID instead of reusing the deterministic copy UID. The rule/source markers remain stable. If a create has an unknown outcome, a missing marker alone does not prove that the provider did not create the event, and the create is not retried blindly.

## Web control plane

Platform mode exposes a public product landing page at `/`, plus the repository-maintained [Privacy Policy](internal/web/legal/privacy.md) at `/privacy` and [Terms of Service](internal/web/legal/terms.md) at `/terms`. These routes contain no connection or calendar state. They can be routed publicly for OAuth application verification while the control plane stays authenticated.

The authenticated React application starts at `/app`. Its primary screen is a unified calendar with day, week, month, and list views, connected-calendar filters, event details, and capability-aware create/edit/reschedule/delete actions. The remaining control-plane pages are:

- Connections — add, reconnect, verify, discover, and safely remove accounts;
- Sync activity — create directed routes, request a pause, stop queued or running jobs, and review sanitized outcomes;
- Deleting a paused rule archives its configuration and retains its history, mappings, and copied calendar events. Deletion is rejected while jobs, pending mutations, or an active dispatch reservation could make the outcome uncertain;
- Settings — installation readiness plus deliberate, short-lived MCP access-key reveal for a single-user installation.

A pause or stop is a request, not proof that a provider request already in flight was revoked. While a worker is still active the UI reports a transitional state; a stopped attempt that may have dispatched a mutation is recorded as unknown and blocks automatic retry. A lost stopped worker is also treated as unknown. Reservations and pending mappings are never cleared by pause, stop, or rule deletion. An active global dispatch reservation conservatively blocks rule deletion and additional real sync jobs. Older running workers do not understand the stop-request schema and must not overlap an upgrade to it. This behavior does not resolve existing unknown provider outcomes.

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

### Recovering paused calendar reads

The calendar error banner offers **Resume sync** for parked sources. One click queues the affected selected calendars through the existing authenticated refresh API, using saved encrypted credentials. It does not open provider sign-in or request an app-specific password. A queued request is not reported as a successful sync; cached events remain visible until the worker completes recovery. **Reconnect** on the Connections page remains a separate action for replacing credentials.

Apple calendars parked after an `auth` error are automatically checked every 15 minutes while their connection is connected and the calendar is readable. The retry deadline is durable, survives worker restarts and daily projection-window changes, and existing parked Apple auth states are eligible after upgrade. Other parked errors require explicit recovery. With strict Gateway preparation enabled, fresh scoped Graph evidence also restarts calendars parked on auth or permission. While waiting, the UI keeps the access error visible, shows the next check and last successful sync, and checks cached status once a minute. Resume does not enable paused event-copy rules or change notification policy.

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
| `ENABLE_V2` | `false` | Enables typed internal REST routes under `/api/v2` |
| `EVENT_READ_MODEL_ENABLED` | `false` | Enables the platform event read model and background publications. Set to `true` to serve `/mcp`. |
| `TOKEN_DIR` | `/app/data` | Legacy standalone OAuth token-file directory |

Migration 007 records Google primary-calendar discovery. Migration 030 adds strict MCP grants, event publications, and write receipts. Use a binary that supports the stored schema. Restoring an older schema requires a separate recovery plan.

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

Cached events may include `self_response_status` for the connected account’s Google primary calendar, confirmed by provider discovery, when exactly one attendee is marked as self and has a recognized response: `accepted`, `declined`, `tentative`, or `needsAction`. This identifies the connected account, which is not necessarily the owner of a shared calendar. Other Google calendars, Microsoft, Apple, missing, and ambiguous responses omit the field. Existing calendar records default to unknown until the Google connection is verified again; discovery does not automatically run as part of the schema migration. Consumers can hide only `declined` and keep unknown responses visible. No attendee list or attendee identity is exposed. The response is as current as the cached source; consult the existing source freshness indicators. Older cached entries without the necessary attendee data remain unknown until refreshed.

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

Connect to `/mcp` using Streamable HTTP and the Calendar API key. The trailing-slash form serves the same catalog. MCP requires platform storage, its encryption key, and `EVENT_READ_MODEL_ENABLED=true`. Run `calendar worker` to verify rights and publish event snapshots. Direct MCP clients and proxies use the same contract.

### Event reads and coverage

`get_events` requires RFC3339 `start` and `end`. Omit `calendar_ids` to use the enabled and readable platform selection with the existing fan-out exclusions, or pass up to 16 unique canonical IDs. `page_size` defaults to 25 and caps each page at 50 before enforcing the 16 KiB SDK result budget. Continue with the same arguments and the opaque `next_page_token`. Timed events sort first by start, followed by all-day events by date. The date filter uses the requested offsets for all-day boundaries and requires a nonempty civil-date range. Source coverage requires the sync window to span both the timed interval and those complete civil days in the source timezone. The cache can change between pages.

The response describes coverage and freshness per source. Fan-out omits sources with expired grants and marks the response incomplete. A terminal page is complete only when every selected range is covered and the traversal has no known omissions.

### Local cache and worker

`list_calendars`, `get_calendar_capabilities`, `get_events`, `get_event`, `get_event_instances`, and `search_events` read local grants and publications. These reads do not call Google, Microsoft Graph, or CalDAV, refresh an OAuth token, or enqueue work. The worker refreshes grants and publications independently of client requests.

- `get_events` returns compact summaries and per-source coverage. Point reads, recurrence reads, and search use the publication for full event data. Search matches a case-insensitive substring in authorized titles, descriptions, or locations. Omitted search bounds use the cache window.
- Missing or indeterminate local evidence is exposed as an unavailable result or partial coverage. A successful empty source can still count as covered. The final page reports `complete=true` only when the entire requested selection is covered.
- The publication queue follows completed ordinary sync generations. It runs up to three publications per worker, with one active attempt per source and a 45-second deadline per attempt. The worker checks the grant, sync generation, range, and write fence before accepting a publication.
- Google reads series and expanded instances separately, with up to 250 results per provider page during publication. It reuses recurring masters within the same snapshot. Apple can fall back from a failed full CalDAV report to inventory and object reads on the configured origin. The fallback preserves escaped paths and rejects ambiguous inventory or missing objects in strict mode. Microsoft Graph can establish cancellation coverage for a series from a fresh series-master cancellation list. A missing event or a delta removal alone leaves deletion coverage incomplete.

Rights checks target five-minute intervals. Each calendar proof lasts 15 minutes from the start of its last successful check. An inconclusive Apple or Graph calendar probe retains only that calendar’s previous deadline while successful siblings renew independently. A check already in progress before expiry can keep that proof available for up to 60 additional seconds while it retains its lease. A failed or denied attempt ends the extension. A new check started after expiry cannot extend the expired proof. Event fields have a separate 15-minute publication limit. Event fan-out omits expired grants. Calendar listing returns a readiness error when it cannot certify the complete inventory. Apple closes the prior grant on confirmed account authentication failure and removes private access immediately when it observes a different collection owner. A fresh Graph proof triggers a replacement sync after an authorization failure.

MCP cursors bind the tool, arguments, verified server credential, and the source grant and content versions. Restart at the first page if any of those change. Mutation fences hide an affected publication until the background refresh catches up.

MCP writes reserve a durable operation before sending the request to the provider. A confirmed provider result can be read by event ID while the full snapshot catches up. Recovery reads the stored receipt or verifies a provider marker for create, supported import, and direct update. A dedicated background loop releases a write with inconclusive evidence after five minutes, records an unknown outcome, and schedules fresh snapshots for its affected calendars. A separate loop reads provider markers, so a slow marker lookup cannot delay expiration. The receipt transaction checks the deadline after acquiring its database lock. Recovery never replays a provider mutation.

The [warmup report](docs/ai-plans/2026-11-19-strict-mcp-warmup.html) records the Apple fallback diagnostics, Google page-size comparison, and Gateway coverage checks. A five-run production sample for the same 27-event day had a median of 145 ms directly against the Calendar API on the VPS and 1.93 s for all three pages through the Gateway connector. The latter includes three request round trips and connector overhead.

### Tools

Calendar MCP exposes 12 tools with typed lifecycle contracts.

| Tool | Purpose |
|---|---|
| `list_calendars` | List calendars across connected accounts |
| `get_calendar_capabilities` | Exact operations, fields, scopes, and notification policies |
| `get_events`, `get_event` | Typed recurrence-aware event reads |
| `get_event_instances` | Bounded recurring instances |
| `search_events` | Case-insensitive search over locally authorized event fields |
| `create_event` | Typed event creation with notification policy |
| `update_event`, `delete_event` | Series, single-instance, and supported following mutations |
| `respond_to_event` | RSVP through Google |
| `move_event` | Move supported Google events between calendars |
| `import_event` | Import an external event into Google using `ical_uid` |

Example arguments for `get_events`:

```json
{
  "calendar_ids": ["google:synthetic-account:primary"],
  "start": "2026-10-24T00:00:00+02:00",
  "end": "2026-10-25T00:00:00+02:00",
  "page_size": 25
}
```

Read `items`, `complete`, `sources`, and `next_page_token` from the structured result. Continue with the same filters. A partial empty page does not establish that the calendar has no events.

Example arguments for `create_event` on an isolated test calendar (a write operation):

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

### Cache storage and authorization

Migration 029 separates authorization revisions from token refresh timestamps/ciphertext. Google verification closes and purges old projection before constructing a snapshot-bound provider, then publishes complete role discovery atomically. The existing worker makes one automatic attempt for each eligible legacy Google account, at most one account per cycle. Failure, cancellation or a crash stays closed until explicit reconnect; UI/native cached data can remain unavailable until reconnect and fresh sync. Google reconnect requires its own refresh token before modifying an existing account. Ordinary refresh assumes the same grant when scope evidence is absent; explicit insufficient scope evidence closes the current grant. Lossless concurrent external refresh-token rotation is not guaranteed.

A newly listed Google calendar on a verified account receives live metadata. Reconnect the account to verify the new calendar's cached detail access. Listing preserves existing projections and does not restart legacy bootstrap.

Before a separately authorized activation, stop/quiesce every old API, worker and cache writer. Already running old binaries are not fenced by startup schema checks. Do not mix old unfenced writers with the new reader. Keep schema 029 with a compatible binary when disabling the feature; an older binary rejects the newer schema. Deployment, Gateway ACL/schema changes and client migration are separate operations.

Local synthetic verification uses the actual Store, OAuth/factory/services, middleware and MCP SDK with fake HTTP and disposable databases. See the [revision 6 implementation and evidence](docs/ai-plans/2026-11-16-calendar-synthetic-pagination-pilot.html#revision-6). The historical harness in `scripts/verify_cached_event_pages_runtime.go` requires the `calendar_mcp_legacy_fixture` build tag, never starts `runtime.Serve`, and accepts only explicit disposable database URLs. Its full-result baseline is a harness-only wrapper around the existing cached Store read, not a replacement production MCP contract.

SQLite selects at most `page_size+1` rows before building guarded summary JSON, while excluding series masters before the limit. The final rights check validates all expected source tuples in one fresh transaction after packing; duplicate tickets retain their original meaning. These changes add no cache, index, migration or pool capacity. See the [full traversal comparison](docs/ai-plans/2026-11-17-cached-traversal-performance.html#implementation-results) for paired before/after measurements and limits. `scripts/verify_cached_event_pages_comparison.py` compares frozen binaries on isolated fixtures; `scripts/measure_cached_page_sql.go` measures the Store SQL phase separately.

SQLite now reads timed/date page ranges through existing covering indexes with native cursor bounds. A same-snapshot preflight preserves the previous query's malformed timestamp handling. Full cached UI reads release SQL rows before decoding when the complete raw result fits a per-call 8 MiB/8192-row buffer; overflow uses streaming, and PostgreSQL keeps its streaming path. The buffer is never reused between requests. The [range and contention results](docs/ai-plans/2026-11-17-sqlite-range-and-contention.html#implementation-results) compare the unchanged pool before, after SQL changes, and after both changes, including large-payload fallback limits.

### Upgrading MCP clients

- Move clients on `/mcp/compat` and `/mcp/modern` to `/mcp`. The former paths return 404. Refresh tool discovery, remove `_v2` suffixes from migrated calls, and verify each input schema.
- Migrate legacy event lists and `get_cached_events` to `get_events`. Use `calendar_ids`, RFC3339 bounds, and the new page and coverage fields. Clients that need the dedicated native summary API can keep using `GET /api/native/v1/cached-events` with its own token.
- Remove `MCP_CATALOG_MODE`, `GATEWAY_CACHED_READS_ONLY`, and `GATEWAY_CACHED_READS_PREPARE` from configuration. During migration, the old strict values are accepted with a warning. Old compatibility and preparation-only values fail startup with migration guidance. `ENABLE_V2` remains available for REST.
- Verify clients through their normal read path after the worker publishes current data. Proxy ACLs and deployment stay under their own operational controls.

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

`GET /api/v2/availability/events` is an additional read-only route for clients
such as Book that need a complete availability check for one calendar. It uses
the same REST authentication. Supply an exact account-scoped `calendar_id`,
RFC 3339 `start` and `end`, and `view=expanded`. For example:

```text
GET /api/v2/availability/events?calendar_id=google%40account-id%3Acalendar-id&start=2030-01-02T09%3A00%3A00Z&end=2030-01-02T18%3A00%3A00Z&view=expanded
```

The ID must identify the connected account and the physical calendar; ambiguous
legacy aliases such as `google:primary` and account-scoped `primary` are not
accepted. Use IDs from the calendar catalog rather than constructing or guessing
them. The service expands recurring events and drains provider pagination
internally. Clients must not supply `page_token`: the route returns the complete
bounded result or an error, rather than a partial list. Consumers should still
check the response's `complete` and `sources` fields and treat errors or an
incomplete source as unavailable, not as an empty calendar.

All-day events contain date-only values without a proven timezone. The route
includes them conservatively whenever a plausible calendar timezone could overlap
the requested interval; clients must not treat those dates as precise UTC
boundaries. Pagination and provider reads do not create a snapshot against
external calendar edits. Existing V2 event routes and MCP catalogs retain their
contracts; this endpoint is additive.

`GET /api/v2/availability/calendars?provider=google` lists enabled,
account-scoped calendars for exactly one provider (`google`, `microsoft`, or
`apple`), using the same REST authentication. A successful response contains
`provider`, `complete: true`, and a non-null `calendars` array. An empty array
means the platform database confirms no configured account for that provider,
or all of its known calendars are disabled. Every configured account for the
selected provider must be connected and successfully listed, and every enabled
registered calendar must appear in the provider result; errors never return a
partial list. Other providers are not contacted. The route requires
the platform database to confirm provider/account identity and does not create
discovery records: a newly seen unregistered calendar makes the catalog unavailable until
registered by the normal discovery flow; disabled registered calendars are omitted. A provider token may still be refreshed by
the existing OAuth token source. The legacy catalog, MCP catalogs and write
checks are unchanged. Clients must not use a one-provider list as evidence
that an omitted provider has no busy events.

Keep this listener internal unless an external client explicitly requires it.

## Standalone REST

Without `DATABASE_URL`, `calendar serve` requires an explicit `REST_LISTEN_ADDR` and provider credentials from environment variables or legacy token files:

```bash
API_KEY=change-me \
REST_LISTEN_ADDR=127.0.0.1:8081 \
GOOGLE_CLIENT_ID=... \
GOOGLE_CLIENT_SECRET=... \
GOOGLE_REFRESH_TOKEN=... \
./calendar serve
```

This profile provides REST only. For MCP, use the platform installation with storage and the worker.

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

## Provider discovery performance

The regular provider registry used by REST and discovery reads connected accounts concurrently with at most four provider
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
go run -tags calendar_mcp_legacy_fixture scripts/measure_calendar_listing.go
go build -tags calendar_mcp_legacy_fixture -o /tmp/calendar-listing-fixture scripts/measure_calendar_listing.go
python3 scripts/verify_calendar_listing_gateway.py
```

The historical integration script additionally needs the local gateway test binary at
`/tmp/mcp-gateway-read-batch`; it creates disposable synthetic configuration and
terminates its fixture processes. Production deployment is a separate action.

### Read-only cached source diagnostics

`GET /api/native/v1/cached-events` adds nullable RFC3339 UTC `last_success_at`
and `next_sync_at`, nullable `coverage_complete`, and optional allowlisted
`error_code` to each source. Coverage is calculated against the requested interval
only when the current window finished syncing (ready or degraded). Pending,
syncing, failed and parked windows report unknown even if an older window once
succeeded; unknown remains
null. Error codes are transient, rate_limited, auth, permission, unsupported, and
protocol. Raw provider messages and credentials are excluded.

These fields read existing stored sync status only. They do not call providers,
change the database schema, trigger synchronization, or change READ_ONLY_TOKEN
permissions. Existing event items and calendar selection remain compatible.
