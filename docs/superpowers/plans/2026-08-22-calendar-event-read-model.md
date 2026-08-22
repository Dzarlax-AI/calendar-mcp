# Calendar Event Read Model Implementation Plan

> **Execution:** Choose inline or delegated execution based on scope and explicit authorization. Steps use checkbox (`- [ ]`) syntax when tracking is useful.

**Goal:** Serve the browser calendar from a durable Postgres event read model that is kept current with provider-specific incremental synchronization.

**Architecture:** Providers remain the source of truth. A worker maintains bounded, UI-ready event projections and opaque provider cursors in Postgres; the browser reads only the projection after a warm-up gate. Google uses a token-compatible unbounded initial feed projected locally to a daily UTC-anchored 365/730-day window, Microsoft uses range-bound `calendarView/delta`, and Apple uses `sync-collection` when supported with a safe cursorless replacement fallback.

**Tech Stack:** Go 1.25+, PostgreSQL/SQLite migrations, Google Calendar API v3, Microsoft Graph v1.0, CalDAV/WebDAV, React Query, FullCalendar, Docker Compose.

---

## Locked scope and operating targets

- UI event reads move to the read model; MCP and internal REST remain direct-provider reads in this release.
- Default materialized window: 365 days behind and 730 days ahead, configurable with `EVENT_CACHE_LOOKBACK_DAYS` and `EVENT_CACHE_LOOKAHEAD_DAYS`.
- Freshness target: Google/Microsoft 60 seconds; Apple 5 minutes. Serve stale data with an explicit source status rather than blocking the whole calendar.
- Postgres is the only cache/store; do not add Redis.
- Provider writes remain synchronous and authoritative. Successful create/update/delete operations immediately patch the projection and enqueue a reconciliation sync.
- Notifications and invitations remain disabled by default; event synchronization is read-only at providers.
- Webhooks are a follow-up after polling is proven. They will wake a sync, never replace periodic reconciliation.
- Feature flag: `EVENT_READ_MODEL_ENABLED=false` by default. Production enablement happens only after all readable calendars have completed an initial sync.

## Model-led execution strategy

The implementation treats minimum Sol usage as a hard constraint. At least 90% of delegable implementation and verification work goes to Terra/Luna; Sol owns no implementation lane and is used only for named high-risk gates and separately authorized production actions. This is a work-allocation target, not a guaranteed billing ratio.

Use exact model overrides with context-free forks: `gpt-5.6-sol` at high effort for the root coordinator, `gpt-5.6-terra` at medium effort for Google/Microsoft and high effort for storage/Apple/worker/web, and `gpt-5.6-luna` at medium effort for fixtures and high effort for the bounded React lane. If a small model cannot satisfy its focused tests after two correction turns, Sol reassesses the brief or takes over that narrow blocker rather than spending unlimited retries.

| Workstream | Primary model | Effort | Sol responsibility |
|---|---|---:|---|
| Schema v2 and storage primitives | Terra | high | Freeze invariants; review migration safety and transaction boundaries |
| Sync coordinator implementation | Terra | high | Define the shared contract first; review cursor/lease semantics |
| Google adapter | Terra | medium | Review 410 reset and token secrecy |
| Microsoft adapter | Terra | medium | Review delta-link validation and fixed-window behavior |
| Apple adapter | Terra | high | Review Family Sharing fallback, concurrency, and object replacement |
| Provider fixtures and repetitive edge-case tests | Luna | medium | Check fixture coverage against the contract |
| Worker integration and configuration | Terra | high | Review lease interaction and activation risk |
| Browser API/read-path integration | Terra | high | Review write ambiguity and API compatibility |
| React freshness UX and frontend tests | Luna | high | Review API/type alignment and user-visible failure states |
| Cross-package integration and full verification | Terra | high | Sol reviews only unresolved high-risk integration findings |
| Commit and deployment | Sol | high | Exception justified by consequential Git/production authority; only after explicit permission |

### Execution waves and concurrency

Only genuinely disjoint file owners run in parallel. The maximum wave is three implementation agents plus the Sol coordinator.

1. **Wave 0 — Terra-high contract freeze:** confirm the task plan, shared types, schema invariants, feature-flag behavior, and exact file ownership. The root coordinator prepares self-contained agent briefs; no delegated implementation starts before this checkpoint. Sol is used only if Terra reports an unresolved cross-cutting risk.
2. **Wave 1 — Terra storage lane:** one Terra agent owns Tasks 1–2. A separate Terra-high review freezes schema/storage before downstream agents receive the storage API.
3. **Wave 2 — provider lanes in parallel:** three Terra agents independently own Google, Microsoft, and Apple production adapters. A Luna follow-up may extend fixtures/tests only after each adapter's production diff is stable. Provider agents may not edit shared contracts or another provider package.
4. **Wave 3 — application lanes in parallel:** one Terra agent owns coordinator/worker integration, one Terra agent owns application/web APIs, and one Luna agent owns frontend UX. They begin from the same Sol-reviewed integration SHA and have disjoint file lists.
5. **Wave 4 — Terra-high synthesis:** Terra reviews every diff, resolves integration changes, runs focused and full suites, and updates the execution record. Sol handles only named unresolved high-risk findings; no agent summary is accepted as verification evidence.
6. **Wave 5 — Sol production gate:** only Sol may commit, push, migrate, enable the worker/feature flag, deploy, or inspect production credentials/state, and only with the corresponding explicit user permission.

### Agent brief and handoff contract

Every delegated task uses `fork_turns="none"` and a bounded, self-contained prompt to avoid paying for unrelated conversation history. The brief must contain:

```text
Base revision: <Sol-reviewed commit SHA>
Plan section: <exact Task and steps>
Allowed files: <exclusive file list>
Forbidden: shared contract edits, unrelated refactors, commit, push, deploy, secrets
Required checks: <exact focused commands and expected behavior>
Return: root cause/approach, changed files, test output, uncertainties, remaining risks
```

Small-model agents must stop and report when the frozen interface is insufficient; they must not silently broaden it. Sol independently reads the actual diff, checks file ownership, reruns tests, and either integrates or returns a narrowly scoped correction. Agents do not spawn further agents unless the user separately authorizes it.

## File structure

**Create**

- `internal/storage/migrations/postgres/002_event_read_model.sql` — event projection, provider object inventory, sync state, and indexes.
- `internal/storage/migrations/sqlite/002_event_read_model.sql` — SQLite-equivalent schema for local/test operation.
- `internal/storage/event_cache.go` — transactional upsert/delete/query and sync-state lease operations.
- `internal/storage/event_cache_test.go` — dialect-neutral storage behavior tests.
- `internal/eventsync/service.go` — provider-agnostic initial/incremental sync coordinator.
- `internal/eventsync/service_test.go` — replay, cursor, generation sweep, and failure tests.
- `internal/google/event_sync.go` and `internal/google/event_sync_test.go` — Google `syncToken` adapter.
- `internal/microsoft/event_sync.go` and `internal/microsoft/event_sync_test.go` — Graph `calendarView/delta` adapter.
- `internal/apple/event_sync.go` and `internal/apple/event_sync_test.go` — WebDAV sync-collection and cursorless replacement fallback adapter.

**Modify**

- `internal/storage/store.go` — sequential migrations through schema version 2.
- `internal/storage/models.go` — cached event and calendar sync-state models.
- `internal/calendar/event_sync.go` — opaque incremental-sync capability contract.
- `internal/config/config.go` — feature flag, cache window, and provider polling intervals.
- `internal/runtime/worker.go` — claim and execute due calendar syncs alongside existing rule jobs.
- `internal/runtime/serve.go` — construct/inject the read model without starting background work in the web process.
- `internal/application/events.go` — optional cached list path and write-through projection updates.
- `internal/web/api.go` — cached UI reads, source freshness, and manual refresh endpoint.
- `internal/web/server.go` — route registration and no-store response policy.
- `frontend/src/lib/types.ts` and `frontend/src/lib/api.ts` — sync status response types and refresh request.
- `frontend/src/features/calendar/CalendarPage.tsx` — stale/syncing/failed source status and refresh affordance.
- `personal_ai_stack/deploy/calendar-mcp/docker-compose.yml` — enable the existing worker only after a production preflight.
- `personal_ai_stack/deploy/calendar-mcp/.env.example` — document non-secret read-model settings.

### Task 1: Add schema version 2 and migration sequencing

**Files:** migration files, `internal/storage/store.go`, `internal/storage/models.go`, storage tests.

**Execution owner:** Terra, high effort. **Sol gate:** approve schema invariants and additive rollback before integration.

- [x] **Step 1: Write failing migration tests**

Add tests that create a version-1 database, run `Migrate`, and assert version 2 plus the new tables. Also assert that a fresh database applies versions 1 and 2 exactly once.

```go
func TestMigrateUpgradesVersionOneToEventReadModel(t *testing.T) {
    store := openVersionOneStore(t)
    if err := store.Migrate(t.Context()); err != nil { t.Fatal(err) }
    if got := schemaVersion(t, store); got != 2 { t.Fatalf("schema version = %d", got) }
    assertTableExists(t, store, "cached_events")
    assertTableExists(t, store, "calendar_sync_state")
    assertTableExists(t, store, "calendar_sync_objects")
}
```

- [x] **Step 2: Verify the test fails**

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/storage -run 'TestMigrate.*EventReadModel' -count=1`

Expected: FAIL because schema version 2 and its tables do not exist.

- [x] **Step 3: Add sequential migrations and schema**

Set `SchemaVersion = 2` and apply every missing embedded migration in order. Version 2 defines:

```sql
CREATE TABLE cached_events (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  event_id TEXT NOT NULL,
  source_object_id TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  payload_json JSONB NOT NULL,
  start_at TIMESTAMPTZ,
  end_at TIMESTAMPTZ,
  start_date TEXT,
  end_date TEXT,
  deleted BOOLEAN NOT NULL DEFAULT FALSE,
  sync_generation BIGINT NOT NULL,
  provider_updated_at TIMESTAMPTZ,
  synced_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (calendar_id, event_id)
);
CREATE INDEX cached_events_timed_range_idx ON cached_events(calendar_id, start_at, end_at) WHERE deleted = FALSE;
CREATE INDEX cached_events_date_range_idx ON cached_events(calendar_id, start_date, end_date) WHERE deleted = FALSE;
CREATE INDEX cached_events_source_object_idx ON cached_events(calendar_id, source_object_id);

CREATE TABLE calendar_sync_state (
  calendar_id TEXT PRIMARY KEY REFERENCES calendars(id) ON DELETE CASCADE,
  strategy TEXT NOT NULL,
  cursor TEXT NOT NULL DEFAULT '',
  window_start TIMESTAMPTZ NOT NULL,
  window_end TIMESTAMPTZ NOT NULL,
  generation BIGINT NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  next_sync_at TIMESTAMPTZ NOT NULL,
  last_started_at TIMESTAMPTZ,
  last_success_at TIMESTAMPTZ,
  last_error_code TEXT,
  lease_owner TEXT,
  lease_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE calendar_sync_objects (
  calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  object_id TEXT NOT NULL,
  etag TEXT NOT NULL,
  sync_generation BIGINT NOT NULL,
  PRIMARY KEY (calendar_id, object_id)
);
```

SQLite uses `TEXT` JSON/timestamps and integer booleans while preserving the same logical columns and keys.

- [x] **Step 4: Verify storage migrations**

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/storage -count=1`

Expected: PASS for fresh, upgrade, and idempotent migration paths.

### Task 2: Implement transactional projection storage and leases

**Files:** `internal/storage/event_cache.go`, models, storage tests.

**Execution owner:** Terra, high effort, same lane as Task 1 to avoid conflicting storage edits. **Sol gate:** inspect all transaction/cursor paths and rerun storage tests.

- [x] **Step 1: Write failing behavior tests**

Cover timed/all-day overlap queries, idempotent replay, object replacement, tombstones, full-sync generation sweep, cursor advancement, lease exclusion, stale lease recovery, and source status.

```go
func TestReplaceEventPageAndAdvanceCursorAtomically(t *testing.T) {
    cache := newTestEventCache(t)
    page := EventSyncBatch{Upserts: []calendar.EventV2{timedEvent("event-1")}, NextCursor: "cursor-2"}
    if err := cache.ApplyPage(t.Context(), state("google", 7), page, true); err != nil { t.Fatal(err) }
    assertCachedEventIDs(t, cache, "event-1")
    assertCursor(t, cache, "cursor-2")
}
```

- [x] **Step 2: Verify tests fail**

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/storage -run 'Test.*Event|Test.*SyncState' -count=1`

Expected: FAIL because projection APIs do not exist.

- [x] **Step 3: Implement storage APIs**

Expose focused methods rather than raw SQL from the sync service:

```go
type CachedEvent struct { CalendarID, EventID, SourceObjectID, ETag, PayloadJSON string; StartAt, EndAt *time.Time; StartDate, EndDate string; Deleted bool; Generation int64; ProviderUpdatedAt *time.Time; SyncedAt time.Time }
type CalendarSyncState struct { CalendarID, Strategy, Cursor, Status string; WindowStart, WindowEnd time.Time; Generation int64; NextSyncAt time.Time; LastStartedAt, LastSuccessAt *time.Time; LastErrorCode, LeaseOwner string; LeaseUntil *time.Time }
type SyncWindow struct { Start, End time.Time }
type EventSyncBatch struct { Upserts []calendar.EventV2; DeletedEventIDs, DeletedObjectIDs []string; Objects []SyncObject; NextCursor string }
type SyncObject struct { ObjectID, ETag string }

func (s *Store) EnsureCalendarSyncStates(ctx context.Context, now time.Time, window SyncWindow) error
func (s *Store) ClaimDueCalendarSync(ctx context.Context, workerID string, now, leaseUntil time.Time) (*CalendarSyncState, error)
func (s *Store) ApplyEventSyncPage(ctx context.Context, state CalendarSyncState, page EventSyncBatch, final bool) error
func (s *Store) FailCalendarSync(ctx context.Context, state CalendarSyncState, code string, next time.Time) error
func (s *Store) ListCachedEvents(ctx context.Context, calendarIDs []string, start, end time.Time) ([]calendar.EventV2, []calendar.SourceStatus, error)
func (s *Store) UpsertCachedEvent(ctx context.Context, event calendar.EventV2, syncedAt time.Time) error
func (s *Store) DeleteCachedEvent(ctx context.Context, ref calendar.EventRef, syncedAt time.Time) error
```

Cursor changes and final generation sweep must be in the same transaction. Failed or cancelled pages must leave the previous cursor authoritative.

- [x] **Step 4: Verify storage behavior**

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/storage -count=1`

Expected: PASS.

### Task 3: Define the provider incremental-sync contract

**Files:** `internal/calendar/provider.go`, `internal/eventsync/service.go`, service tests.

**Execution owner:** Terra, high effort, freezes the public types and implements the coordinator and tests. **Sol exception gate:** only if replay, reset, cancellation, or lease-loss semantics remain ambiguous after Terra review.

- [x] **Step 1: Write failing coordinator tests**

Test initial sync, multi-page incremental sync, replay after failure, invalid-cursor reset, partial provider failure, and per-provider scheduling.

```go
type EventSyncRequest struct { CalendarID string; WindowStart, WindowEnd time.Time; Cursor, PageToken string; Generation int64 }
type EventSyncPage struct { Upserts []EventV2; DeletedEventIDs, DeletedObjectIDs []string; Objects []SyncObject; NextPageToken, NextCursor string; ResetRequired, Complete bool }
type EventSyncProvider interface { SyncEvents(context.Context, EventSyncRequest) (EventSyncPage, error) }
```

- [x] **Step 2: Verify tests fail**

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/eventsync -count=1`

Expected: FAIL because the coordinator is absent.

- [x] **Step 3: Implement the coordinator**

`eventsync.Service.RunOne` builds the routed provider, drains pages, applies each idempotently, advances the cursor only on the final page, performs full-sync sweep only after success, and schedules the next attempt. Authentication/schema errors park the source; transient provider errors use the configured bounded retry delay, capped at the provider polling interval unless the provider supplies `Retry-After`.

- [x] **Step 4: Verify coordinator behavior**

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/eventsync -count=1`

Expected: PASS with no cursor advancement on injected page failure.

### Task 4: Implement provider adapters

**Files:** provider sync files and tests.

**Execution owners:** three parallel Terra lanes, one provider per agent. Luna may add fixture-heavy edge-case tests in a later, non-overlapping pass. **Sol gate:** provider-by-provider diff and focused test review before integration.

- [x] **Google adapter**

Initial request uses an unbounded token-compatible `events.list` feed, `singleEvents=true`, and `showDeleted=true`, then projects locally into the daily UTC-anchored window. Persist `nextSyncToken`; subsequent requests use the opaque `syncToken` and page token. Treat HTTP 410 as `ResetRequired`, clear only that calendar projection after a replacement full sync completes, and never expose tokens in logs. This follows Google's documented full/incremental model and reset behavior.

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/google -run 'TestSync' -count=1`

- [x] **Microsoft adapter**

Use `/me/calendars/{id}/calendarView/delta` for the fixed rolling window. Validate every `@odata.nextLink`/`@odata.deltaLink` host with the existing Graph URL validator and store the final delta URL as an opaque cursor. Rebase with a full sync when the configured window changes.

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/microsoft -run 'TestSync' -count=1`

- [x] **Apple adapter**

Attempt RFC 6578 `sync-collection` per calendar. When unsupported or malformed, use the safe cursorless replacement fallback with bounded-concurrency GETs over current ICS resources. The contract has no previous inventory for a prior-inventory ETag optimization. One `.ics` object replacement atomically replaces all materialized instances associated with its `source_object_id`.

Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/apple -run 'TestSync' -count=1`

Expected for all adapters: exact pagination, deletions, opaque cursors, recurrence instances, and reset behavior pass with `httptest` providers.

### Task 5: Run calendar synchronization in the worker

**Files:** `internal/config/config.go`, `internal/runtime/worker.go`, worker tests.

**Execution owner:** Terra, high effort. **Sol gate:** review worker leases, interaction with rule jobs, and production activation controls.

- [x] Add validated configuration defaults:

```text
EVENT_READ_MODEL_ENABLED=false
EVENT_CACHE_LOOKBACK_DAYS=365
EVENT_CACHE_LOOKAHEAD_DAYS=730
EVENT_SYNC_GOOGLE_INTERVAL=60s
EVENT_SYNC_MICROSOFT_INTERVAL=60s
EVENT_SYNC_APPLE_INTERVAL=5m
```

- [x] Extend each worker cycle to ensure states for connected/readable calendars, claim at most one due calendar sync at a time, execute it with a renewable 15-minute lease, then continue existing rule jobs.
- [x] Verify disconnected/error connections are not synced; removing a calendar cascades its projection; reconnect schedules an immediate full sync.
- [x] Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/runtime ./internal/connections -count=1`

Expected: PASS including concurrent-worker claim exclusion and stale lease recovery.

### Task 6: Switch only the browser read path and make writes coherent

**Files:** application/web API files and tests.

**Execution owner:** Terra, high effort. **Sol gate:** review API compatibility, ambiguous write outcomes, and the no-direct-provider-read invariant.

- [x] Write API tests proving the feature flag selects the cached path, no provider list call occurs, incomplete sources remain visible, and an unwarmed cache returns `syncing` rather than a misleading empty-success response.
- [x] Under `EVENT_READ_MODEL_ENABLED=true`, `/api/ui/events` queries `ListCachedEvents`; MCP and REST handlers retain their current direct-provider behavior.
- [x] Extend UI source status with `status`, `last_success_at`, `stale`, and a safe `error_code`.
- [x] After a successful provider mutation, upsert the returned event or remove the deleted event and set `next_sync_at=now`. If local projection update fails after the provider accepted the write, return the provider result with a reconciliation warning and enqueue sync; never retry the mutation blindly.
- [x] Add `POST /api/ui/calendars/{id}/refresh` to set `next_sync_at=now`; it must not call the provider from the web request.
- [x] Run: `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/application ./internal/web ./internal/restapi ./internal/mcpserver -count=1`

Expected: PASS and direct-provider MCP/REST regression tests unchanged.

### Task 7: Add freshness UX without blocking the calendar

**Files:** frontend types/API/calendar page and tests.

**Execution owner:** Luna, high effort, using the frozen API response contract. **Sol gate:** inspect type alignment, error states, and rendered behavior before integration.

- [x] Add a compact status row: `Syncing`, `Updated <relative time>`, `Some calendars are stale`, or `N calendars failed`.
- [x] Keep cached events rendered during background refresh. Do not replace usable stale data with a full-screen loader.
- [x] Manual Refresh enqueues synchronization and polls event/source status; it does not issue parallel direct-provider reads.
- [x] Invalidate the current event query after local mutations while keeping optimistic drag revert behavior.
- [x] Run:

```bash
cd frontend
npm test -- --run
npm run typecheck
npm run build
```

Expected: all frontend tests, typecheck, and production build pass.

### Task 8: Warm-up, deployment, and cutover

**Files:** personal stack compose and env example; the HTML plan execution record after completion.

**Execution owner:** Sol only. Small models receive no production access and perform no Git/external mutations.

- [ ] Before activation, verify production schema backup/recovery, all existing sync rules are paused or explicitly intended, notifications remain disabled, and persistent Postgres data is backed up.
- [ ] Deploy schema version 2 with `EVENT_READ_MODEL_ENABLED=false`; start `calendar-worker` from the reviewed immutable image.
- [ ] Observe initial sync to completion for every connected readable calendar. Verify row counts, duplicate keys, recurrence samples, deletions, and source freshness without printing event content.
- [ ] Compare cached and direct-provider counts for isolated test ranges on Google, Microsoft, and Apple. Differences require explanation before cutover.
- [ ] Enable `EVENT_READ_MODEL_ENABLED=true` on `calendar-serve`, restart only `calendar-serve`, and verify health plus the authenticated calendar path.
- [ ] Measure browser API latency and provider request volume; acceptance target is warm `/api/ui/events` p95 under 300 ms on the VPS and no provider requests on ordinary view navigation.
- [ ] Run final repository checks:

```bash
GOCACHE=/private/tmp/calendar-mcp-go-build go test -race -count=1 ./...
GOCACHE=/private/tmp/calendar-mcp-go-build go vet ./...
GOCACHE=/private/tmp/calendar-mcp-go-build go build -o /private/tmp/calendar-mcp-server ./cmd/server
```

- [ ] Prepare commits or deployment only with explicit permission.

## Execution record — 2026-08-22

Tasks 1–7 were implemented locally. Task 8 (warm-up, production deployment, provider/data comparison, feature-flag cutover, and commit/deployment preparation) was **not executed** and remains an explicit-permission production gate.

Implemented architecture: schema v2; lease/CAS/replacement storage; an optional provider contract and coordinator; a Google token-compatible unbounded feed projected locally to a daily UTC-anchored 365/730-day window; Microsoft delta; Apple sync-collection with a safe cursorless replacement fallback (the contract has no previous inventory, so there is no prior-inventory ETag optimization); parked nonretryable state with reactivation; one worker claim per cycle with a renewable 15-minute lease; a UI-only cached service clone preserving MCP/REST direct-provider behavior; write-through mutations with reconciliation warnings; and source freshness UX with bounded polling.

Review corrections recorded here: Google initial bounds were removed for token compatibility, the window uses UTC-day rebasing rather than per-tick rolling timestamps, and the UI clone preserves existing MCP/REST direct behavior. The final Sol cumulative review also canonicalized deleted CalDAV object identities, classified Google HTTP 403 quota reasons as retryable rate limits rather than permanent permission failures, and surfaced bounded reconciliation warnings in the browser after confirmed provider writes.

Post-PR automated review corrections: cached series masters are retained for reconciliation but excluded from the recurrence-expanded browser response; Apple resources containing multiple master VEVENTs are exposed read-only until member-addressed provider mutations exist; and recurrence-scoped deletes immediately reconcile every visible cached occurrence while preserving returned following-scope events.

CodeRabbit follow-up corrections: write-through generation reads and writes are now one row-locked transaction; Graph invalidates replacement continuation state through the bounded reset path; page/reset capacity limits retry instead of parking; heartbeat shutdown cannot turn a successful final commit into a lease-loss failure; Google requests the supported 2500-item page size; browser polling and warning bounds reset safely; and an opt-in `TEST_POSTGRES_URL` integration path covers production JSONB/timestamp bindings when a test database is available.

Final local checks passed: `GOCACHE=/private/tmp/calendar-mcp-rabbit-race go test -race -count=1 ./...`; `go vet ./...`; server build; frontend Vitest 20/20, typecheck, and production build; `git diff --check`; an independent Terra-high cumulative review (approved); final Sol cumulative review; GitHub Codex review; and CodeRabbit review with the corrections above.

Limitations: this session did not validate a live PostgreSQL `TEST_POSTGRES_URL`, run migration/worker/flag deployment, verify production providers or data, or perform the Task 8 comparisons. Apple fallback refetches all current ICS resources, and a daily Google replacement seed can be expensive. Rollback remains `EVENT_READ_MODEL_ENABLED=false`; schema changes are additive and inert. Terra/Luna executed the primary implementation lanes and reviews; Sol coordinated, performed the cumulative review and narrow corrections, and ran final verification. Git/PR preparation was separately authorized after the implementation gate; no production mutation was authorized or performed.

## Rollback

1. Set `EVENT_READ_MODEL_ENABLED=false` and restart only `calendar-serve`; UI immediately returns to direct-provider reads.
2. Stop `calendar-worker` if event sync causes provider load. Existing cached tables remain inert.
3. Keep schema version 2 in place during rollback; do not destructively downgrade or delete cached rows.
4. Revert the application commit and redeploy the previous immutable image only after explicit approval. Database cleanup is a separate, later operation.

## Acceptance checklist

- Warm UI navigation performs zero provider event-list requests.
- Initial and incremental sync handle additions, updates, cancellations, deletions, pagination, and recurring instances.
- One provider failure does not hide healthy calendars or erase stale usable events.
- Cursor/page replay is idempotent; a crash cannot advance a cursor beyond committed events.
- Google 410, Microsoft invalid delta link, and Apple unsupported sync-token all recover without global cache deletion.
- Successful UI mutations appear immediately and reconcile later without duplicate provider writes.
- No invitations/notifications are emitted by sync or QA.
- MCP and REST behavior remains unchanged in this release.

## Follow-up deliberately excluded

- Google and Microsoft webhook subscriptions, renewal, signature/token validation, and notification ingress.
- Moving MCP/REST reads to the projection.
- Full-text event search and analytics indexes.
- Redis, multi-region replication, offline browser persistence, or conflict-resolution UI.
