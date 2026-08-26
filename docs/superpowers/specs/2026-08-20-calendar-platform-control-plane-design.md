# Calendar Platform Control Plane Design

**Date:** 2026-08-20

**Status:** Approved; implementation plan pending approval

**Implementation plan:** `docs/ai-plans/2026-08-20-calendar-platform-control-plane.html`

**Scope:** Consolidate `calendar-mcp` and `calendar-sync`, then add a single-user web control plane for calendar connections, sync rules, and operations.

## Summary

The calendar services will become one self-hosted calendar platform with one repository, one Go module, one multicall binary, and one OCI image. Production runs the image as two containers:

- `calendar serve` provides the MCP endpoint, internal REST API, OAuth callbacks, and the server-rendered HTMX administration UI.
- `calendar worker` schedules and executes synchronization rules.

Both processes use the same durable database. PostgreSQL is the primary production backend; SQLite remains a supported lightweight option for third-party self-hosters. Book remains a separate product and continues to consume the calendar platform API.

## Goals

- Connect Google Calendar and Microsoft 365 through browser OAuth flows.
- Connect Apple Calendar through CalDAV credentials and an app-specific password.
- Discover calendars and expose their read/write capabilities.
- Configure multiple one-way sync rules while allowing only provider pairs supported by the server.
- Configure independent past lookback and future lookahead depth for each sync rule.
- Initially support Microsoft 365 to Google Calendar synchronization.
- Provide dry runs, manual runs, run history, status, counters, and actionable errors.
- Preserve recurring series, modified occurrences, and cancelled occurrences across providers.
- Encrypt provider credentials at rest for both PostgreSQL and SQLite.
- Preserve runtime isolation between the serving process and the synchronization worker.

## Non-goals

- A multi-user or multi-tenant hosted service.
- Bidirectional synchronization in the initial design.
- A standalone TypeScript frontend or SPA.
- Calendar-grid or calendar-client functionality.
- Moving Book into this repository or image.
- Silently converting unsupported recurring series into independent events.

## Users and Access

Each installation has one administrative user context. The production UI is protected by Authentik ForwardAuth. The application does not maintain its own user accounts, organizations, roles, or tenant identifiers.

OAuth callback routes must remain externally reachable. They are protected with short-lived state, PKCE where the provider supports it, strict redirect validation, and single-use authorization attempts. Successful callbacks return the administrator to the protected UI.

MCP retains API-key authentication. The service-to-service REST API remains available only on the internal network.

## Runtime Architecture

### Shared artifact

One Go binary named `calendar` is built into one OCI image. It exposes two commands:

```text
calendar serve
calendar worker
```

The same immutable image digest is used for both containers. The containers retain separate process lifecycles, logs, health checks, networks, environment variables, and volumes.

### Serve process

The serve process is responsible for:

- MCP and internal calendar REST APIs;
- provider OAuth initiation and callback handling;
- the Go template and HTMX administration UI;
- calendar discovery and connection verification;
- CRUD for sync rules;
- enqueueing manual and dry-run jobs;
- schema migrations under a database migration lock.

The serve process never executes a synchronization inline with an HTTP request.

### Worker process

The worker process is responsible for:

- polling due rules and explicitly queued jobs;
- acquiring a per-rule execution lock;
- provider reads and writes;
- maintaining event and series mappings;
- recording run progress, counters, warnings, and errors;
- periodic full reconciliation in addition to incremental synchronization.

The worker checks the database schema version at startup and refuses to run against an incompatible schema. It does not apply migrations.

## Persistence

The application uses a storage interface implemented for PostgreSQL and SQLite. Business logic must not depend on PostgreSQL-only behavior.

- PostgreSQL is the recommended production backend.
- SQLite uses WAL mode, a bounded busy timeout, foreign keys, and a shared persistent volume between the serve and worker containers.
- SQLite installations support one worker process only.
- PostgreSQL uses row-level job claiming so multiple workers cannot execute the same rule concurrently, even though horizontal worker scaling is not an initial goal.
- The selected backend is configured through `DATABASE_URL`.

### Data model

`connections`

- Provider, display label, connection status, granted scopes, last verification result, and encrypted provider credentials.

`calendars`

- Connection reference, provider calendar identifier, display metadata, timezone, and discovered capabilities such as readable, writable, and recurrence support.

`sync_rules`

- Source and target calendar references, enabled or paused state, schedule interval, past lookback, future lookahead, supported direction, recurrence policy, notification policy, and last scheduling state.

`sync_jobs`

- Scheduled, manual, reconciliation, or dry-run request; job state; claim metadata; retry availability; and timestamps.

`sync_runs`

- Rule and job reference, trigger type, start and finish time, outcome, aggregate counters, sanitized warnings, and sanitized error summary.

`event_mappings`

- Rule reference, source and target provider identifiers, object kind, source series identifier, original occurrence time where applicable, content fingerprint, last-seen marker, and reconciliation state.

`oauth_attempts`

- Short-lived, single-use OAuth state and PKCE material with expiry. Completed and expired attempts are removed.

## Credential Security

Provider application client IDs and client secrets are installation configuration supplied through environment variables. Provider access tokens, refresh tokens, and Apple app-specific passwords are stored in the database using authenticated encryption.

- `CALENDAR_ENCRYPTION_KEY` supplies the application master key.
- Encrypted values include a versioned envelope so key rotation and format changes can be added without changing domain records.
- Credentials are decrypted only at the provider boundary and never returned to browser clients.
- Missing or invalid encryption keys fail closed. Calendar operations are disabled and the UI reports a configuration problem without exposing credential data.
- Logs, sync history, metrics, and error summaries must not contain tokens, passwords, authorization codes, raw provider payloads, attendee data, or private event descriptions.
- Losing the encryption key requires reconnecting providers; this consequence is documented for self-hosters.

## Provider Connections

### Google Calendar

The administrator selects Connect, completes the browser OAuth flow, and returns to the Connections screen. The platform stores the resulting token set, verifies the account, and discovers calendars and capabilities.

### Microsoft 365

The flow mirrors Google but uses the configured Microsoft tenant policy and Graph scopes. The initial supported sync source is a Microsoft calendar.

### Apple Calendar

The administrator enters an Apple identifier and app-specific password over the protected UI. The platform verifies CalDAV access before storing the encrypted credentials. Existing Apple Family Sharing fallback behavior remains part of the provider adapter.

Connections can be verified and reconnected. A connection cannot be deleted while a sync rule references one of its calendars; the dependent rules must first be paused and removed or redirected.

## Synchronization Rules

The data model permits multiple one-way rules. The server publishes a capability matrix and only offers combinations that are implemented and tested. The first supported direction is Microsoft 365 to Google Calendar.

Rule creation follows this sequence:

1. Select a readable source connection and calendar.
2. Select a writable target connection and calendar.
3. Validate the provider-pair capability.
4. Configure the schedule, past lookback, future lookahead, and recurrence behavior.
5. Review a safety summary.
6. Save the rule in paused state.
7. Run a read-only dry run.
8. Enable the rule only after a successful dry run.

Safe defaults are mandatory:

- attendees are not copied;
- invitations and provider notifications are not sent;
- the source is never mutated;
- a newly created rule starts paused;
- destructive target changes are included in dry-run output before enablement.
- the default depth is zero days of past lookback and 14 days of future lookahead.

An enabled one-way rule owns the synchronized title, time, location, description, recurrence, and cancellation state in target mirror events. Manual edits to these fields in the target may be overwritten and this behavior is disclosed in the UI.

## Recurrence Model

The provider-neutral event model distinguishes:

- standalone event;
- recurring series master;
- unchanged occurrence;
- modified exception;
- cancelled occurrence.

It retains the provider object identifier, series identifier, original occurrence time, recurrence rule, source timezone, all-day semantics, and cancellation state.

For Microsoft 365 to Google Calendar:

1. A source series master creates one target recurring series.
2. A modified source occurrence updates the corresponding target instance.
3. A cancelled occurrence removes or cancels only the corresponding target instance.
4. Cancelling the master removes the target series.
5. Master and exception mappings are stored separately.

Objects created by the platform receive private synchronization metadata containing the rule and source identity needed for recovery. After a timeout or ambiguous provider response, the worker searches for this metadata before retrying a write, preventing duplicate mirrors.

Unsupported recurrence rules block dry run and prevent a rule from being enabled. They are neither skipped nor silently materialized as independent events: the rule must be supported or removed before enablement. An explicit materialization fallback may be designed later and is out of scope here.

Deletion is driven by provider tombstones, explicit cancellation state, or full reconciliation. Absence from a bounded time window is not proof of deletion.

## Operations UI

The UI uses Go templates and HTMX. It has five primary areas:

- **Dashboard:** connection health, active rules, last successful activity, and errors needing attention.
- **Connections:** provider connection, reconnection, verification, calendar discovery, capabilities, and safe removal.
- **Sync Rules:** rule list, paused or active state, source and target, schedule, lookback and lookahead depth, dry run, manual run, edit, and disable.
- **Runs:** filterable history and a sanitized detail view with trigger, duration, counters, warnings, and errors.
- **Settings:** database and encryption readiness, provider application configuration readiness, and operational defaults without revealing secrets.

The UI is an operational control plane, not a calendar grid.

## Job Execution and Failure Handling

- One rule cannot execute concurrently with itself.
- A failure in one rule does not stop other rules.
- Retries are bounded and limited to operations that are safe or can be recovered through synchronization metadata.
- Ambiguous creates and updates are verified before another write is attempted.
- Authentication, authorization, validation, and unsupported-capability errors are not retried automatically.
- Repeated failures place a rule in an attention-required state without deleting mappings.
- Rate limits, transient provider failures, credential failures, and unsupported recurrence are distinct user-visible categories.
- Dry runs perform provider reads and local planning only; they never mutate source, target, mappings, or synchronization cursors.

## Delivery Milestones

### Milestone A: platform and control plane

- Consolidate repositories and Go modules.
- Build the multicall binary and shared image.
- Introduce PostgreSQL and SQLite storage implementations and migrations.
- Add encrypted provider connections and OAuth flows.
- Add the HTMX control plane, sync rules, jobs, dry runs, and run history.
- Import the existing one-way sync behavior without changing its event semantics.

### Milestone B: recurrence correctness

- Add the series-aware provider-neutral model.
- Add provider delta and tombstone handling.
- Add series master and exception mappings.
- Implement recurring-series creation, modification, cancellation, and full reconciliation.
- Complete recurrence and timezone validation before treating the consolidated platform as finished.

## Migration and Rollback

Migration from the existing deployment is staged:

1. Back up the current environment files, OAuth token files, and sync state file.
2. Create new platform connections through the approved OAuth flows; do not import legacy provider credentials or token files.
3. Convert the current source and target configuration into one paused sync rule.
4. Import the existing event mappings with an explicit legacy marker.
5. Run the new worker in dry-run mode and compare its plan with the current deployment.
6. Stop the old sync worker only after the new dry run is accepted.
7. Start the new worker with writes enabled and verify the user-visible calendar path.
8. Retain the old images and state files until the rollback window is closed.

Rollback restores the previous immutable images, environment files, token files, and sync state. Provider grants are not revoked during migration, so rollback does not require reauthorization.

## Test Strategy

- Run the same storage contract suite against PostgreSQL and SQLite.
- Test migrations from an empty database and from every supported prior schema version.
- Use provider contract fixtures for Google, Microsoft, and Apple adapters.
- Cover recurring series, moved occurrences, cancelled occurrences, series updates, all-day events, timezones, and DST transitions.
- Verify idempotent recovery after timeouts and ambiguous provider responses.
- Prove that dry run executes no provider or database mutations.
- Run an integration topology containing the serve process, worker process, and selected database.
- Manually verify Authentik protection, OAuth callbacks, reconnect, revoke, invalid encryption key handling, and recovery after provider outages.
- After deployment, verify both container health and the complete user path: connect, discover calendars, dry run, enable, synchronize, inspect history, and pause.

## Approved Decisions

- Single-user installation protected by Authentik.
- Full browser OAuth for Google and Microsoft; app-specific password for Apple.
- Multiple one-way rules with a server-controlled capability matrix.
- Microsoft 365 to Google Calendar is the first supported direction.
- Operational status, history, manual runs, and dry runs are initial-scope capabilities.
- Each rule has independent lookback and lookahead depth, defaulting to 0 days back and 14 days forward.
- Recurrence includes series masters, modified occurrences, and cancellations.
- Go templates and HTMX provide the UI.
- PostgreSQL is the recommended backend and SQLite is supported for self-hosters.
- Credentials are encrypted at the application layer.
- One repository, Go module, binary, and image run as separate serve and worker containers.
- Book remains a separate consumer.
