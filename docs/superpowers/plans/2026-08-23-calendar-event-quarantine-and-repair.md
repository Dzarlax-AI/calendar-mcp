# Calendar Event Quarantine and Repair Implementation Plan

> **Execution:** Delegated Terra/Luna lanes by default; minimize Sol usage. Sol is reserved for the explicitly requested final cross-cutting plan review. No production mutation occurs until the user approves the HTML plan and separately authorizes implementation/deploy.

**Goal:** Keep a calendar usable when one Google event or Apple `.ics` resource is malformed by quarantining only that object, advancing the safe provider cursor, and offering a bounded repair path.

**Architecture:** Provider adapters emit object-scoped warnings with stable identity and ETag. Storage persists a safe quarantine row atomically with valid cache mutations and cursor advancement. The coordinator retries at most one due object under the existing calendar lease, while the API/UI exposes only aggregate repair metadata and a repair action; cached events remain readable.

**Tech Stack:** Go, SQLite/Postgres migrations, provider adapters (Google Calendar API and CalDAV), React/TypeScript/Vitest, Docker/GitHub Actions.

---

## Model-led execution strategy

| Workstream | Primary model | Effort | Owned files | Required checks | Coordinator gate |
|---|---|---:|---|---|---|
| Contract, migration, storage/quarantine transaction | Terra | high | `internal/calendar/event_sync.go`, `internal/storage/models.go`, `internal/storage/event_cache.go`, `internal/storage/migrations/{sqlite,postgres}/003_event_sync_quarantine.sql`, `internal/storage/store.go` | SQLite tests, Postgres integration when configured, race | Root verifies cursor/sweep/lease invariants |
| Google and Apple object repair | Terra | high | `internal/google/event_sync.go`, `internal/google/event_sync_test.go`, `internal/apple/event_sync.go`, `internal/apple/event_sync_test.go` | provider fixtures for valid/invalid/404/auth/rate-limit cases | Contract must be frozen first |
| Coordinator integration | Terra | high | `internal/eventsync/service.go`, `internal/eventsync/service_test.go` | warning + terminal cursor, repair backoff, lease-loss tests | Root inspects transaction behavior |
| Safe API/UI status and repair action | Luna | medium | `internal/web/api.go`, `internal/web/server.go`, `internal/web/api_test.go`, `frontend/src/lib/{types,api,calendar}.ts`, `frontend/src/features/calendar/CalendarPage.tsx`, `frontend/src/styles/app.css`, frontend tests | frontend typecheck/tests, API safe-field tests | No provider IDs, ETags, URLs, payloads, cursors, or secrets exposed |
| Integrated review and release gate | Root | medium | cross-cutting diff | full race/vet/build, manual Google + Apple smoke | Root owns integration; the explicitly requested Sol plan review is complete |

Allocation target: implementation lanes are Terra/Luna-owned; Sol owns no implementation lane. Root owns integration and release decisions.

### Execution waves

1. **Contract freeze — Terra high:** finalize typed warning/repair interfaces, quarantine schema, cursor and sweep invariants. No implementation starts before root accepts the contract.
2. **Parallel provider lanes — Terra:** Google and Apple repair methods use the frozen contract and own disjoint provider files.
3. **Coordinator/storage integration — Terra high:** persist warnings, advance terminal cursor, exclude quarantined objects from sweeps, and run one bounded repair under the calendar lease.
4. **API/UI lane — Luna:** consume aggregate diagnostics and expose a safe repair action after backend response shape is stable.
5. **Integrated verification — root:** inspect all diffs and run focused and full checks; the explicitly requested Sol plan review is already complete and no second Sol pass is implied.
6. **Release — root:** only after explicit user approval; migration/image rollout and real provider-path verification are separate gates. No second Sol pass is implied by this plan.

## Invariants

1. A terminal page with valid siblings advances the main cursor even when it contains known object warnings.
2. Per-page valid mutations and quarantine upserts happen atomically; terminal-page cursor advance, source aggregate, and replacement sweep join that terminal transaction.
3. Active quarantined objects are excluded from full replacement cleanup.
4. A successful normal upsert, repair upsert, or provider-confirmed deletion clears quarantine atomically; no speculative clearing. Active unresolved rows have no TTL.
5. A warning refreshes `last_seen_at`; an unchanged ETag preserves backoff, while a changed ETag resets attempts and makes repair due.
6. Repair uses the existing calendar lease, never changes the main cursor, and is limited to one due object per run.
7. Missing stable identity remains a hard protocol error; it cannot be safely quarantined.
8. Cached events remain readable throughout, and public responses contain only aggregate diagnostics.

## Task 1: Freeze provider-neutral contract and schema

**Execution owner:** Terra high. **Files:** `internal/calendar/event_sync.go`, `internal/storage/models.go`, both `003_event_sync_quarantine.sql` migrations, `internal/storage/store.go`.

- [ ] Add `ETag` to `calendar.EventSyncWarning`.
- [ ] Add `EventSyncObjectRepairProvider`, `EventSyncObjectRepairRequest`, and `EventSyncObjectRepairResult` with explicit outcomes: `replace_membership`, `absent_from_projection`, `provider_deleted`, and `still_quarantined`; do not use a boolean that conflates provider deletion with an empty frozen window.
- [ ] Add `MaxObjectRepairsPerRun` policy defaulting to `1`.
- [ ] Add `calendar_sync_quarantine` with `(calendar_id, object_id)` primary key, active status, safe error code, ETag, timestamps, next repair time, and attempt count; active unresolved rows never expire. Add `(calendar_id, next_repair_at)` index for both SQLite and Postgres. Only resolved/history rows may be expired in a later, separate scope.
- [ ] Add storage models and narrow methods for listing due quarantine, atomically upserting warnings, and clearing only after confirmed repair/delete.
- [ ] Verify migration registration and schema version checks in `internal/storage/store.go`.

Expected checks: migration unit tests and SQLite schema test pass; no raw provider payload, cursor, account identifier, or secret column exists.

## Task 2: Make Google and Apple warnings repairable

**Execution owner:** Terra high. **Files:** `internal/google/event_sync.go`, `internal/google/event_sync_test.go`, `internal/apple/event_sync.go`, `internal/apple/event_sync_test.go`.

- [ ] Google malformed item with non-empty ID returns warning `{Code: protocol, ObjectID: item.Id, ETag: item.Etag}`; ID-less item remains hard protocol failure.
- [ ] Implement Google object repair with `Events.Get`: valid event returns upserts, `404` returns confirmed deletion, malformed time/recurrence returns protocol warning, auth/rate-limit/transport returns typed hard failure.
- [ ] Google cancellation or provider-confirmed 404/410 confirms deletion and clears quarantine; moved-out-of-window is `absent_from_projection`, not provider deletion, and has an explicit cache-membership outcome.
- [ ] Apple warning carries canonical resource identity and the response ETag internally (captured from the repair response, never copied from the caller ETag); malformed body/recurrence is repairable, while auth/permission/rate-limit/transport remains hard failure. Missing response ETag uses conservative unchanged-ETag behavior and never resets attempts speculatively.
- [ ] Implement Apple repair through existing canonical path, isolated GET/parser, and recurrence membership validation; 404/410/empty membership confirms deletion.
- [ ] Do not change direct-read endpoints or write paths.

Expected checks: provider fixture tests cover valid repair, malformed object, confirmed deletion, auth, rate-limit, 5xx, and transport outcomes.

## Task 3: Integrate quarantine with coordinator and storage transaction

**Execution owner:** Terra high. **Files:** `internal/eventsync/service.go`, `internal/eventsync/service_test.go`, `internal/storage/event_cache.go`, `internal/storage/event_cache_test.go`.

- [ ] Replace attempt-wide `degraded` cursor suppression with object-scoped quarantine batches.
- [ ] On every page, atomically apply that page's valid mutations and warning upserts under the lease. On the terminal page, atomically apply terminal mutations, terminal warnings, aggregate state, quarantine-aware sweep, and `NextCursor`.
- [ ] Ensure replacement sweep never deletes active quarantined object membership in either `cached_events` or `calendar_sync_objects`, including warnings first seen on an intermediate page.
- [ ] Before ordinary provider sync, claim at most one due quarantine under the same calendar lease; apply the explicit repair outcome atomically.
- [ ] Auth/permission repair failures park/fail the source; rate-limit/transient failures reschedule only the repair without blocking the main feed; protocol warnings remain quarantined.
- [ ] Clear rows only on confirmed upsert or provider-confirmed delete; dedupe repeated warnings by object and ETag.

Expected checks: tests prove cursor advances with warning, old poisoned membership survives sweep, repair success/delete clears quarantine, repair failure does not block normal sync, and lease loss causes no second mutation.

## Task 4: Expose safe aggregate diagnostics and repair action

**Execution owner:** Luna medium, after backend contract is stable. **Files:** `internal/web/api.go`, `internal/web/server.go`, `internal/web/api_test.go`, `frontend/src/lib/types.ts`, `frontend/src/lib/api.ts`, `frontend/src/lib/calendar.ts`, `frontend/src/features/calendar/CalendarPage.tsx`, `frontend/src/styles/app.css`, frontend tests.

- [ ] Add `degraded` source status and aggregate `quarantined_count`, `repairable_count`, and `last_observed_at` only; never expose object IDs, ETags, URLs, cursors, raw errors, iCalendar, or credentials.
- [ ] Add calendar-scoped CSRF-protected `POST /api/ui/calendars/{calendar_id}/diagnostics/repair`. HTTP performs no provider I/O; it atomically makes at most one active quarantine due (or wakes the calendar), and returns `202 accepted`, `409 conflict` for an active repair/lease, or `422 unsupported`. No diagnostic token or expiry endpoint is needed.
- [ ] Render `ready + quarantine` as amber “Events pending repair”; retain red semantics for auth/permission/parked failures.
- [ ] On accepted repair, invalidate events, reset bounded polling, disable duplicate clicks, and distinguish confirmed/ambiguous outcomes.
- [ ] Add tests for status normalization, safe-field filtering, CSRF/URL encoding, mixed status precedence, and cached-event visibility.

Expected checks: frontend typecheck and Vitest pass; API responses contain no provider payload or identifier leakage.

## Task 5: Integrated verification and rollout

**Execution owner:** Root. **Files:** plan artifact and release notes only until implementation approval.

- [ ] Inspect all delegated diffs and run `go test ./internal/{google,apple,eventsync,storage,web} -count=1`.
- [ ] Run `GOCACHE=/private/tmp/calendar-mcp-go-build go test -race -count=1 ./...`, `go vet ./...`, frontend typecheck/tests, and `git diff --check`.
- [ ] Run Postgres integration as a release blocker for this production Postgres deployment; do not approve rollout with migration/CAS/sweep coverage unavailable.
- [ ] After separate implementation approval, deploy one immutable image containing serve and worker with migration compatibility; verify both health endpoints, one real Google repair, one Apple repair, cached events, and no new provider auth/protocol loop.
- [ ] Use an expand-only migration. Drain old serve/worker, wait for leases, migrate once, then start the same-digest serve/worker. Pre-build and name a v3-schema-compatible rollback digest; do not assume the old v2 binary can restart after migration.

## Scope phases and resolved decisions

- **Phase 1 (core correctness):** contract, schema, storage/coordinator/provider quarantine, cursor/sweep/lease invariants, and aggregate API status.
- **Phase 2 (user repair UX):** calendar-scoped manual repair action and amber UI. It remains separately testable and does not block Phase 1 read-model correctness.
- Public terminology is resolved to `degraded` + “events pending repair”; generic `failed` remains for auth/permission/parked sources.
- Repair is bounded automatic (one due object per normal run) plus calendar-scoped manual wake-up; no opaque diagnostic token is required.
- Active unresolved quarantine rows do not expire. Retention/history is explicitly out of scope.

## Approval gate

Implementation was explicitly approved and completed locally. The requested Sol plan review is complete; PR creation, merge, deployment, Rabbit review, and external notification remain separate gates.

## Implementation status

- Backend contract, schema v3, quarantine storage, cursor/sweep behavior, coordinator repair scheduling, Google repair, and Apple repair are implemented.
- API/UI preserve `degraded`, keep cached events visible, redact provider details, and reuse the provider-free calendar refresh action for bounded repair retry.
- Verified: full Go test, Go vet, frontend Vitest (21 tests), frontend production build, and `git diff --check`.
- Sol code review found and the implementation fixed three issues: active quarantine now controls terminal status, Apple malformed repair preserves the response ETag, and still-quarantined repair schedules bounded retry.
- Production Postgres integration remains a rollout blocker until run with `TEST_POSTGRES_URL`.
