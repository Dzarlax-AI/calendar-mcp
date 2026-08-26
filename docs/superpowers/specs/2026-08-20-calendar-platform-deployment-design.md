# Calendar Platform Production Deployment Design

**Status:** Approved design, implementation still gated
**Date:** 2026-08-20
**Target:** Personal VPS at `65.109.0.90`

## Goal

Deploy the calendar platform control plane without interrupting the existing MCP consumers. The web UI and OAuth callbacks will use `https://calendar.dzarlax.dev`, while MCP clients will keep using `https://mcp.dzarlax.dev/calendar` before and after cutover.

The deployment must reuse the existing Traefik, Authentik, PostgreSQL, and Docker networks. It must not build images on the VPS, expose the worker or internal REST API publicly, enable real synchronization before isolated-calendar QA, or remove the legacy service during the initial rollout.

## Confirmed Current State

- Production currently runs one legacy `calendar-mcp` container from a mutable `latest` reference. It serves MCP at `mcp.dzarlax.dev/calendar` through a Traefik path rewrite.
- The deployed image predates the merged control-plane implementation. No platform worker is running.
- The local deployment draft already anticipates a separate `calendar.dzarlax.dev` UI host, a `calendar-worker`, Authentik ForwardAuth, and the shared PostgreSQL network, but it has not been applied to production.
- The shared PostgreSQL initialization source defines `calendar_user` and a `calendar` schema, but the live role and schema are absent because first-start initialization does not rerun on the existing volume.
- Authentik has no Calendar application. The existing proxy outpost and Traefik ForwardAuth middleware will be reused.
- `calendar.dzarlax.dev` has been created by the user, but DNS resolution was not yet visible from the deployment workstation at the latest check. Deployment waits for propagation and successful HTTPS routing.
- Existing standalone OAuth tokens are not imported into platform connections. Accounts will be authorized again through the new UI.
- Existing MCP consumers are numerous. The public URL and the legacy API key must remain compatible during a user-controlled migration period.
- A diagnostic output exposed the current calendar credentials in the session. Existing API and provider credentials are therefore treated as temporary compatibility credentials and must be replaced before the legacy deployment is retired.

## Selected Architecture

One Compose project will temporarily run three services:

1. **`calendar-mcp` (legacy)**
   - Keeps the current image digest, environment, token files, and low-priority MCP router.
   - Continues serving existing clients throughout shadow deployment and validation.
   - Does not connect to the new platform database or run platform synchronization.

2. **`calendar-serve` (new control plane)**
   - Uses the CI-built control-plane image pinned by immutable digest.
   - Runs schema migration, web UI, OAuth callbacks, MCP, and health checks.
   - Connects to the shared PostgreSQL instance through the isolated `calendar` schema.
   - Receives the public UI host immediately; its MCP router is added only at cutover.

3. **`calendar-worker` (new worker)**
   - Uses the exact same immutable image as `calendar-serve`.
   - Connects only to the `infra` Docker network.
   - Has no Traefik labels or published ports.
   - Starts only after connection discovery and shadow validation. All rules remain paused until isolated sync QA.

The initial Compose update must add the two new services without recreating or renaming the running legacy service.

## Routing and Authentication

### Web UI and OAuth

- `https://calendar.dzarlax.dev` routes to `calendar-serve:8080`.
- Protected UI routes use the existing `authentik-auth` ForwardAuth middleware.
- Google and Microsoft callback paths use dedicated higher-priority Traefik routers that bypass ForwardAuth. OAuth state and PKCE remain the callback security boundary.
- The application listener is reachable only from Docker networks; no host port is published.
- Traefik supplies `X-authentik-username`; the application keeps `UI_TRUST_FORWARD_AUTH=true` and `UI_ALLOW_UNAUTHENTICATED=false`.

Authentik configuration will be created through its supported admin UI or API, not by direct database or Django ORM writes. The Calendar proxy provider/application will use `https://calendar.dzarlax.dev` as its external host, attach to the existing proxy outpost, and bind access only to the user's existing Authentik identity or an already-defined exact policy. No new group name will be invented.

### MCP

- The public URL remains `https://mcp.dzarlax.dev/calendar`.
- Traefik continues rewriting `/calendar` to the application's internal `/mcp` endpoint.
- Before cutover, the existing router targets `calendar-mcp`.
- At cutover, a second router with the same host/path and a higher explicit priority targets `calendar-serve`.
- The low-priority legacy router remains active for immediate rollback.
- MCP does not use Authentik. It remains protected by API keys.

## Temporary Dual API-Key Support

The application will support:

- `API_KEY`: the new primary key;
- `API_KEY_LEGACY`: the existing temporary compatibility key.

Authentication accepts either exact value and rejects all others. Comparisons must not introduce timing-sensitive plain string behavior if the existing middleware already uses a safer comparison. Neither key, its prefix, nor request authorization data may be logged or shown in the UI.

`API_KEY_LEGACY` is optional and disabled by default. It will be removed only after the user confirms that all consumers have migrated. Removing it, revoking old credentials, and deleting the legacy service are separate production actions outside the initial deployment.

## Provider Credential Rotation

The new control plane receives credentials that are independent from the legacy service:

- a new Google OAuth client and secret with the exact new callback URI;
- parallel Microsoft application credentials with the exact new callback URI;
- a new Apple app-specific password entered through the UI.

The old credentials and refresh tokens remain available only to the legacy container during the compatibility period. New Google and Microsoft accounts are authorized through the control-plane UI and stored encrypted in PostgreSQL. Old token files and environment refresh tokens are not copied into the platform database.

Credentials must not be pasted into repository files, plans, command output, or chat. If an Authentik API token is required, it will be passed through a temporary approved mechanism and will not be retained in artifacts or shell history.

## PostgreSQL Layout

The deployment reuses `infra-postgres-1` and the existing `aistack` database, with:

- role `calendar_user`;
- schema `calendar` owned by `calendar_user`;
- `calendar_user` default `search_path` restricted to `calendar`;
- no create permission in `public`;
- a unique generated password stored only in the production calendar environment.

An idempotent provisioning script will live with the calendar deployment rather than relying on the already-consumed global first-start initialization script. It will be executed once with administrative PostgreSQL access after a verified backup. The script must be safe when the role or schema already exists and must not embed a password in the repository.

The container-to-container connection may retain `sslmode=disable` only because it remains on the private local Docker network. No PostgreSQL port or password is exposed publicly. A future remote/shared-network database would require `sslmode=verify-full` and a trusted CA.

## Rollout Phases

### Phase 0: Build and preflight

- Implement and test optional legacy API-key support.
- Prepare the three-service Compose configuration with immutable image references.
- Verify the main-branch CI image and resolve its actual registry digest.
- Prepare the database provisioning script and rollback commands.
- Confirm current production configuration, image, backups, container health, and resource headroom.

### Phase 1: Infrastructure preparation

- Wait until `calendar.dzarlax.dev` resolves to the VPS.
- Verify Traefik can obtain a valid certificate without changing the legacy route.
- Create the Calendar application/provider in Authentik and verify the outpost receives it.
- Back up PostgreSQL, then provision `calendar_user` and the `calendar` schema.
- Create new provider credentials while leaving legacy credentials valid.

### Phase 2: Shadow control plane

- Upload reviewed deployment files without overwriting the server `.env`.
- Add the new environment variables directly on the VPS through an approved secret-safe workflow.
- Pull the pinned image; never build on the VPS.
- Start only `calendar-serve` and verify migration, health, restart count, logs, Authentik redirect, authenticated UI, and OAuth callback routing.
- Connect provider accounts and discover calendars. Do not create enabled sync rules.

### Phase 3: Isolated synchronization QA

- Use empty, dedicated test calendars. Notifications and invitations remain disabled.
- Start `calendar-worker` only after all connections are healthy.
- Confirm the worker is ready and has no unexpected runnable jobs.
- Create rules in the paused state.
- Run dry-run validation first, followed by one explicitly approved real one-way sync between test calendars.
- Verify a normal event, an all-day event, and a recurring event with a moved exception.
- Confirm that no attendees, invitations, or notifications were copied or sent.

### Phase 4: MCP cutover

- Add the higher-priority new MCP router without removing the legacy router.
- Verify MCP initialization, tool discovery, and a read-only calendar query using the new key.
- Repeat the same read-only path using the legacy key.
- Verify the Personal Assistant consumer and any other immediately available client.
- Keep all non-test synchronization rules paused.

### Phase 5: Compatibility period

- Keep the legacy service, router, credentials, and `API_KEY_LEGACY` available.
- The user migrates consumers to the new key over time.
- Do not infer completion from elapsed time. Retirement starts only on the user's explicit command.

## Failure Handling and Rollback

- **DNS or certificate failure:** do not start OAuth setup; legacy MCP is unaffected.
- **Authentik failure:** remove or disable only the Calendar application/provider; do not restart or modify unrelated Authentik applications.
- **Database provisioning or migration failure:** stop the new service, preserve the database, collect sanitized errors, and restore only if the reviewed rollback requires it. Do not delete the schema automatically.
- **OAuth or discovery failure:** leave the worker stopped and the MCP route on legacy.
- **Sync QA failure:** pause the exact rule, stop the new worker if necessary, and do not cut over MCP.
- **Post-cutover MCP failure:** remove the higher-priority new router or recreate only `calendar-serve` from the pre-cutover Compose configuration. The legacy router resumes without URL or client changes.
- Never delete volumes, token files, mappings, database objects, or legacy credentials as an automatic rollback step.

## Verification and Success Criteria

The initial rollout is successful only when all of the following are observed:

- DNS resolves and HTTPS presents a valid certificate for `calendar.dzarlax.dev`.
- Unauthenticated UI access redirects through Authentik; the authorized user reaches the UI; a spoofed identity header cannot bypass the trusted proxy boundary.
- OAuth callback paths reach the application without an Authentik redirect and reject invalid state.
- `calendar-serve` and `calendar-worker` are healthy with zero unexpected restarts and no relevant ERROR/FATAL log entries.
- PostgreSQL objects exist only in the `calendar` schema and the application role cannot create in `public`.
- Provider connections and calendar discovery succeed using newly authorized credentials.
- Isolated sync QA completes without attendees, invitations, or notifications.
- Before cutover, the existing MCP route still reaches legacy.
- After cutover, the unchanged MCP URL succeeds with both primary and legacy keys and rejects an invalid key.
- A real consumer path, including Personal Assistant, succeeds against the new control plane.
- The legacy container remains available for rollback.

## Explicitly Excluded From the Initial Deployment

- Removing `API_KEY_LEGACY`.
- Revoking legacy provider credentials or refresh tokens.
- Deleting the legacy container, token files, or data directory.
- Enabling synchronization for production calendars.
- Importing old OAuth tokens into the platform database.
- Removing the legacy Traefik router.
- Restarting shared Traefik, Authentik, or PostgreSQL containers unless separately approved and proven necessary.

## Implementation Boundary

Implementation requires a separate browser-readable HTML plan and explicit approval. That plan must enumerate the application, deployment, database, Authentik, OAuth-console, test-calendar, cutover, and rollback operations. No production mutation is authorized by this design document alone.
