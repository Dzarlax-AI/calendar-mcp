# Calendar Platform Production Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy the calendar control plane beside the existing MCP service, validate it safely, and cut the unchanged MCP URL over with an immediate legacy rollback path.

**Architecture:** Keep the existing `calendar-mcp` container and low-priority Traefik router intact while adding `calendar-serve` and `calendar-worker` from one immutable CI image. The UI uses `calendar.dzarlax.dev` behind Authentik, both services share the isolated `calendar` PostgreSQL schema, and a higher-priority overlay router performs the later MCP cutover. The new service accepts a new primary API key and one explicitly temporary legacy key.

**Tech Stack:** Go 1.25+, `net/http`, Docker Compose, Traefik 3.6, Authentik 2026.2, PostgreSQL 17, GitHub Actions/GHCR, Cloudflare DNS managed by the user.

---

## File Map

### `calendar-mcp`

- Modify `internal/config/config.go` — load the optional legacy API key.
- Modify `internal/config/config_test.go` — prove environment loading and primary-key validation.
- Modify `internal/auth/middleware.go` — accept either configured key with constant-time comparison.
- Modify `internal/auth/middleware_test.go` — cover primary, legacy, invalid, and fail-closed behavior.
- Modify `internal/mcpserver/server.go` — receive a shared `auth.Options` value.
- Modify `internal/restapi/rest.go` — receive the same authentication options.
- Modify `internal/runtime/serve.go` — construct and pass one API authentication configuration.
- Modify `.env.example` and `README.md` — document the temporary rotation variable and retirement behavior.
- Update `docs/ai-plans/2026-08-20-calendar-platform-production-deployment.html` after implementation with actual evidence.

### `personal_ai_stack/deploy/calendar-mcp`

- Modify `docker-compose.yml` — preserve the legacy service and add shadow `calendar-serve` and `calendar-worker` services.
- Create `docker-compose.cutover.yml` — add only the higher-priority MCP router to `calendar-serve`.
- Create `provision-calendar.sql` — idempotently provision the role, schema, ownership, search path, and public-schema restriction without embedding a password.
- Create `.env.example` only if a non-secret deployment variable reference is needed; never copy it over the server `.env`.

No generated file, credential value, provider token, database password, or resolved API key belongs in either repository artifact.

## Authorization Gates

1. Approval of the HTML plan authorizes local source/config/test changes only.
2. Commit, push, and PR creation require explicit authorization after the diff and tests are shown.
3. PostgreSQL, Authentik, OAuth-console, secret, and container changes require a separate production-shadow approval.
4. Starting the worker and running a real test-calendar synchronization require explicit confirmation that notifications/invites are disabled and the selected calendars are disposable.
5. MCP cutover requires a fresh readiness verdict and explicit cutover approval.
6. Legacy retirement, old-key removal, credential revocation, and legacy-container deletion remain separate actions.

### Task 1: Create an isolated implementation worktree

**Files:** none

- [ ] **Step 1: Preserve the dirty root checkout and refresh remote state**

Run from `/Users/dzarlax/Projects/Code/Personal/calendar-mcp`:

```bash
git status --short --branch
git fetch origin main
```

Expected: the root checkout may remain behind and contain the user's untracked planning artifacts; no stash, checkout, reset, or deletion occurs.

- [ ] **Step 2: Create the implementation worktree from current `origin/main`**

```bash
git worktree add .worktrees/calendar-platform-deployment -b feat/calendar-platform-deployment origin/main
```

Expected: a clean named branch based on the merged control-plane tree.

- [ ] **Step 3: Copy only the approved planning artifacts into the worktree**

Use `apply_patch` to add this implementation plan, the deployment design spec, and the HTML approval artifact to the isolated worktree. Do not copy `.claude/`, mockups, or unrelated untracked files.

### Task 2: Add optional legacy API-key support with TDD

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/auth/middleware.go`
- Modify: `internal/auth/middleware_test.go`
- Modify: `internal/mcpserver/server.go`
- Modify: `internal/restapi/rest.go`
- Modify: `internal/runtime/serve.go`

- [ ] **Step 1: Write failing configuration and middleware tests**

Add to `internal/config/config_test.go`:

```go
func TestLoadReadsLegacyAPIKey(t *testing.T) {
	t.Setenv("API_KEY", "primary")
	t.Setenv("API_KEY_LEGACY", "legacy")
	cfg := Load()
	if cfg.APIKey != "primary" || cfg.LegacyAPIKey != "legacy" {
		t.Fatalf("keys = %q/%q", cfg.APIKey, cfg.LegacyAPIKey)
	}
}
```

Replace the credential table in `TestMiddlewareAcceptsConfiguredCredentials` with cases for `primary` and `legacy`, and construct the middleware with:

```go
Options{APIKey: "primary", LegacyAPIKey: "legacy"}
```

Add a test proving an empty primary key still fails closed even when `API_KEY_LEGACY` is configured in `Config`; the primary key remains mandatory for platform startup.

- [ ] **Step 2: Run the focused tests and verify they fail**

```bash
GOCACHE=/tmp/calendar-mcp-deploy-plan-go-build go test -count=1 ./internal/config ./internal/auth
```

Expected: compile failures because `LegacyAPIKey` does not exist.

- [ ] **Step 3: Implement the minimal configuration and middleware change**

Add `LegacyAPIKey string` next to `APIKey` in `config.Config` and load it with:

```go
LegacyAPIKey: envStr("API_KEY_LEGACY", ""),
```

Add the same field to `auth.Options`. Keep the existing fail-closed branch based on the primary key. Replace the single comparison with:

```go
validPrimary := equal(provided, opts.APIKey)
validLegacy := opts.LegacyAPIKey != "" && equal(provided, opts.LegacyAPIKey)
if !validPrimary && !validLegacy {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return
}
```

This explicit non-empty check is required: without it, an absent credential and an unset legacy key would compare as equal and authenticate accidentally. Add a regression case proving that missing credentials remain unauthorized when `LegacyAPIKey` is empty. Different non-empty values continue to use constant-time comparison.

- [ ] **Step 4: Pass one authentication value through MCP and REST**

Change `mcpserver.Register` to accept `authOptions auth.Options` instead of separate key and unauthenticated arguments. Change `restapi.New` the same way. In `runtime.Serve`, construct:

```go
apiAuth := auth.Options{
	APIKey:               cfg.APIKey,
	LegacyAPIKey:         cfg.LegacyAPIKey,
	AllowUnauthenticated: cfg.AllowUnauthenticated,
}
```

Pass `apiAuth` to MCP and optional REST. Update direct test callers, if any, to pass `auth.Options` explicitly.

- [ ] **Step 5: Run focused and full tests**

```bash
gofmt -w internal/config/config.go internal/config/config_test.go internal/auth/middleware.go internal/auth/middleware_test.go internal/mcpserver/server.go internal/restapi/rest.go internal/runtime/serve.go
GOCACHE=/tmp/calendar-mcp-deploy-plan-go-build go test -race -count=1 ./...
GOCACHE=/tmp/calendar-mcp-deploy-plan-go-build CGO_ENABLED=0 go vet ./...
GOCACHE=/tmp/calendar-mcp-deploy-plan-go-build CGO_ENABLED=0 go build ./...
```

Expected: all packages pass; no credential value appears in output.

- [ ] **Step 6: Document the rotation contract**

Add `API_KEY_LEGACY` to `.env.example` as an empty, temporary compatibility setting. In `README.md`, state that `API_KEY` is mandatory, `API_KEY_LEGACY` is optional, both protect MCP/REST identically, and the legacy value should be removed after all clients migrate.

### Task 3: Prepare the three-service Compose deployment

**Files:**

- Modify: `personal_ai_stack/deploy/calendar-mcp/docker-compose.yml`
- Create: `personal_ai_stack/deploy/calendar-mcp/docker-compose.cutover.yml`

- [ ] **Step 1: Preserve the legacy service verbatim**

Start from the deployed `/root/calendar-mcp/docker-compose.yml`, not the newer local draft. Keep service name and container name `calendar-mcp`, its data mount, current environment, current networks, current low-priority MCP router, and current rewrite middleware unchanged. Replace its mutable image only after resolving and recording the exact currently running digest; do not pull or recreate it during shadow deployment.

- [ ] **Step 2: Add `calendar-serve`**

Use `${CALENDAR_PLATFORM_IMAGE:?Set CALENDAR_PLATFORM_IMAGE to a reviewed immutable digest}` for the image and `command: ["serve"]`. Configure:

```yaml
environment:
  DATABASE_URL: postgres://calendar_user:${CALENDAR_DB_PASSWORD:?Set CALENDAR_DB_PASSWORD}@infra-postgres-1:5432/${POSTGRES_DB:-aistack}?sslmode=disable
  CALENDAR_ENCRYPTION_KEY: ${CALENDAR_ENCRYPTION_KEY:?Set CALENDAR_ENCRYPTION_KEY}
  CALENDAR_PUBLIC_URL: https://calendar.${DOMAIN}
  API_KEY: ${API_KEY:?Set the new primary API key}
  API_KEY_LEGACY: ${API_KEY_LEGACY:-}
  ENABLE_V2: "true"
  UI_TRUST_FORWARD_AUTH: "true"
  UI_ALLOW_UNAUTHENTICATED: "false"
```

Attach `traefik` and `infra`. Add the UI router with `authentik-auth`, plus separate Google and Microsoft callback routers with priority `200` and no ForwardAuth middleware. Bind Traefik to port `8080`. Do not add the new MCP router in the base file.

- [ ] **Step 3: Add `calendar-worker`**

Use the same `${CALENDAR_PLATFORM_IMAGE}` and database/encryption settings, `command: ["worker"]`, only the `infra` network, and the existing worker health check on `127.0.0.1:8082`. Depend on `calendar-serve` health. Do not publish ports or Traefik labels.

- [ ] **Step 4: Create the cutover overlay**

In `docker-compose.cutover.yml`, extend only `calendar-serve` with a router named `calendar-mcp-platform`:

```yaml
services:
  calendar-serve:
    labels:
      traefik.enable: "true"
      traefik.http.routers.calendar-mcp-platform.entrypoints: https
      traefik.http.routers.calendar-mcp-platform.rule: Host(`mcp.${DOMAIN}`) && PathPrefix(`/calendar`)
      traefik.http.routers.calendar-mcp-platform.priority: "300"
      traefik.http.routers.calendar-mcp-platform.tls: "true"
      traefik.http.routers.calendar-mcp-platform.tls.certresolver: letsEncrypt
      traefik.http.routers.calendar-mcp-platform.middlewares: calendar-platform-rewrite
      traefik.http.middlewares.calendar-platform-rewrite.replacepathregex.regex: /calendar(.*)
      traefik.http.middlewares.calendar-platform-rewrite.replacepathregex.replacement: /mcp$$1
      traefik.http.services.calendar-platform-mcp.loadbalancer.server.port: "8080"
      traefik.http.routers.calendar-mcp-platform.service: calendar-platform-mcp
```

Keep the legacy router's lower default priority. Do not reuse its router, middleware, or service names.

- [ ] **Step 5: Validate base and cutover models**

With digest-shaped test values and `.env.example` as a service env file, run:

```bash
docker compose -f personal_ai_stack/deploy/calendar-mcp/docker-compose.yml config --quiet
docker compose -f personal_ai_stack/deploy/calendar-mcp/docker-compose.yml -f personal_ai_stack/deploy/calendar-mcp/docker-compose.cutover.yml config --quiet
```

Expected: both render; base contains one MCP router on legacy, overlay contains the additional priority-300 router on `calendar-serve`; worker has no `traefik` network or labels.

### Task 4: Add idempotent PostgreSQL provisioning

**Files:**

- Create: `personal_ai_stack/deploy/calendar-mcp/provision-calendar.sql`

- [ ] **Step 1: Add a password-safe psql script**

Create:

```sql
\set ON_ERROR_STOP on
\getenv calendar_password CALENDAR_DB_PASSWORD

SELECT format('CREATE ROLE calendar_user LOGIN PASSWORD %L', :'calendar_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'calendar_user')
\gexec

SELECT format('ALTER ROLE calendar_user LOGIN PASSWORD %L', :'calendar_password')
\gexec

CREATE SCHEMA IF NOT EXISTS calendar AUTHORIZATION calendar_user;
ALTER SCHEMA calendar OWNER TO calendar_user;
ALTER ROLE calendar_user SET search_path TO calendar;
REVOKE CREATE ON SCHEMA public FROM calendar_user;
GRANT USAGE, CREATE ON SCHEMA calendar TO calendar_user;
```

The password comes only from the process environment and is never embedded in the file.

- [ ] **Step 2: Test the script twice against disposable PostgreSQL 17**

Start a disposable PostgreSQL 17 container, create database `aistack`, execute the script twice with the same generated test password, then query:

```sql
SELECT rolname, rolconfig FROM pg_roles WHERE rolname = 'calendar_user';
SELECT schema_name, schema_owner FROM information_schema.schemata WHERE schema_name = 'calendar';
SELECT has_schema_privilege('calendar_user', 'public', 'CREATE');
```

Expected: one role, one owned schema, `search_path=calendar`, and `false` for public create privilege. Remove only the disposable container and volume afterward.

### Task 5: Review, publish, and resolve an immutable image

**Files:** application and deployment changes from Tasks 2-4

- [ ] **Step 1: Run the complete local gate**

```bash
git diff --check
GOCACHE=/tmp/calendar-mcp-deploy-plan-go-build go test -race -count=1 ./...
GOCACHE=/tmp/calendar-mcp-deploy-plan-go-build CGO_ENABLED=0 go vet ./...
GOCACHE=/tmp/calendar-mcp-deploy-plan-go-build CGO_ENABLED=0 go build ./...
```

Also render both Compose models and rerun the disposable PostgreSQL provisioning test.

- [ ] **Step 2: Show the cumulative diff and request publication approval**

List application-repository changes separately from `personal_ai_stack` deployment artifacts. Do not stage unrelated root artifacts. Wait for explicit authorization before commit, push, or PR creation.

- [ ] **Step 3: After authorization, create one focused PR**

Use English conventional commits without generated trailers. The PR must explain dual-key compatibility, the unchanged MCP URL, production exclusions, and test evidence. Wait for required CI/review and explicit merge approval.

- [ ] **Step 4: After merge, resolve the CI-built immutable image**

Pull the exact merge-SHA tag on the VPS, then inspect `RepoDigests`. Set `CALENDAR_PLATFORM_IMAGE` to the resulting `ghcr.io/dzarlax-ai/calendar-mcp@sha256:...` reference. Never deploy `latest` and never build on the VPS.

### Task 6: Production read-only preflight and infrastructure preparation

**Files:** reviewed deployment artifacts only

- [ ] **Step 1: Recheck DNS and current service state**

Verify `calendar.dzarlax.dev` resolves to `65.109.0.90`. Record legacy container image digest, health, restarts, networks, Compose file, and sanitized ERROR/WARN counts. Confirm the old MCP initialize/tool path still works before any mutation.

- [ ] **Step 2: Verify backup readiness and create a pre-platform dump**

Confirm free space and existing backup state. Create a mode-`0600` PostgreSQL custom-format dump under a new `/root/calendar-mcp/backups/` directory. Verify the dump with `pg_restore --list`; do not modify or restart `infra-postgres-1`.

- [ ] **Step 3: Provision the role and schema**

Copy only `provision-calendar.sql`, load `CALENDAR_DB_PASSWORD` without printing it, and execute the script inside the existing PostgreSQL container. Verify role settings, schema ownership, public-schema denial, and an empty `calendar` schema.

- [ ] **Step 4: Configure Authentik through supported interfaces**

Create a Proxy Provider/Application named Calendar with external host `https://calendar.dzarlax.dev`, forward-auth mode, and the existing authorization/authentication flows. Attach it to the existing proxy outpost and bind only the user's exact existing identity/policy. Do not use direct ORM/database writes and do not restart Authentik. Verify the outpost receives the application and an unauthenticated root request redirects to Authentik.

- [ ] **Step 5: Create parallel provider credentials**

Create new Google and Microsoft application credentials with these exact callbacks:

```text
https://calendar.dzarlax.dev/oauth/google/callback
https://calendar.dzarlax.dev/oauth/microsoft/callback
```

Create a new Apple app-specific password. Keep all old credentials valid for legacy. Store new values only in the production `.env` or the UI credential flow; never in files copied from the repository.

### Task 7: Shadow-start and validate the control plane

**Files:** `/root/calendar-mcp/docker-compose.yml`, production `.env`

- [ ] **Step 1: Upload reviewed non-secret files**

Copy Compose and SQL artifacts to `/root/calendar-mcp/` without copying, replacing, or displaying `.env`. Compare server/local checksums before applying.

- [ ] **Step 2: Add required environment values safely**

Back up `.env` with mode `0600`, then add the new primary key, temporary legacy key, database password, encryption key, immutable image reference, public URL, and new provider application credentials. Validate variable names and non-empty status without printing values.

- [ ] **Step 3: Pull and start only `calendar-serve`**

```bash
docker compose pull calendar-serve
docker compose up -d --no-deps calendar-serve
```

Do not recreate `calendar-mcp`, start the worker, or use the cutover overlay.

- [ ] **Step 4: Verify the shadow service**

Check container health, image digest, restart count, migration logs, and sanitized ERROR/FATAL counts. Verify:

- internal `/health` returns `200`;
- public root redirects through Authentik;
- authenticated UI loads;
- callback routes reach the application and reject invalid state rather than redirecting to Authentik;
- spoofed identity headers do not bypass ForwardAuth;
- legacy MCP still serves the unchanged public URL.

- [ ] **Step 5: Connect providers and discover calendars**

Connect Google and Microsoft through OAuth and Apple through the UI. Verify stored connection status and calendar discovery without exposing credential payloads. Create no enabled rule.

### Task 8: Start the worker and perform isolated sync QA

**Files:** production state only

- [ ] **Step 1: Confirm safe test calendars**

Identify empty dedicated source and target calendars. Confirm the tested path sends `NotificationsNone`, copies no attendees, and cannot affect a real calendar. Stop if this cannot be proven.

- [ ] **Step 2: Start only the worker**

```bash
docker compose pull calendar-worker
docker compose up -d --no-deps calendar-worker
```

Verify readiness, exact image equality with `calendar-serve`, zero unexpected restarts, and no runnable jobs.

- [ ] **Step 3: Create paused rules and run dry-run QA**

Use the UI to create a one-way test rule with explicit lookback/lookahead and paused state. Run dry-run and inspect sanitized counters. Do not enable production-calendar rules.

- [ ] **Step 4: Run one explicitly approved real test sync**

Create a normal event, an all-day event, and a recurring event with a moved exception in the dedicated source calendar. Enable only the test rule, run once, pause it again, and verify target fidelity plus absence of attendees, invites, and notifications.

### Task 9: Cut over the unchanged MCP URL

**Files:** `docker-compose.cutover.yml`, production state

- [ ] **Step 1: Run a fresh cutover gate**

Require healthy serve/worker containers, successful provider discovery, completed isolated QA, green application CI, clean logs, the running legacy fallback, and explicit user authorization.

- [ ] **Step 2: Add only the priority-300 router**

```bash
docker compose -f docker-compose.yml -f docker-compose.cutover.yml up -d --no-deps calendar-serve
```

Do not recreate legacy or worker.

- [ ] **Step 3: Verify both authentication paths**

Against `https://mcp.dzarlax.dev/calendar`, perform MCP initialize, `tools/list`, and one read-only calendar query with the primary key, then repeat with the legacy key. Verify an invalid key returns `401`. Do not print keys or authorization headers.

- [ ] **Step 4: Verify a real consumer**

Run a read-only Personal Assistant calendar request and confirm it reaches the new service. Check Traefik/service logs using only request status and route metadata, not calendar payloads.

- [ ] **Step 5: Prove rollback remains available**

Confirm the legacy container is still running and its low-priority router remains loaded. Document the rollback command that recreates only `calendar-serve` from base Compose without the cutover overlay.

### Task 10: Record results and preserve the compatibility state

**Files:**

- Modify: `docs/ai-plans/2026-08-20-calendar-platform-production-deployment.html`

- [ ] **Step 1: Record actual evidence**

Add final image digests, container states, migrations, DNS/TLS, Authentik, OAuth, test-calendar, MCP dual-key, real-consumer, and rollback verification. Record failures and skipped checks explicitly.

- [ ] **Step 2: Leave retirement actions undone**

Do not revoke old credentials, remove `API_KEY_LEGACY`, delete the legacy container/data, remove its router, or enable real-calendar sync rules. Report them as user-controlled follow-ups.
