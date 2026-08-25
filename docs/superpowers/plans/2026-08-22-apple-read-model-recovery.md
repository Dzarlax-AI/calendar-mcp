# Apple Read-Model Recovery Implementation Plan

> **Execution:** Choose inline or delegated execution based on scope and explicit authorization. Minimize Sol usage; assign implementation to Luna/Terra and justify every Sol exception. Steps use checkbox (`- [ ]`) syntax when tracking is useful.

**Goal:** Make Apple read-model synchronization resilient to malformed individual iCalendar resources without deleting the last valid snapshot or falsely reporting the source as ready.

**Architecture:** Keep provider truth and the existing direct path unchanged. Extend the provider-neutral sync contract with per-page recoverable warnings and an attempt-level degraded result: valid objects are applied, but if any page in the attempt was degraded, the authoritative cursor is not advanced, replacement generations are not swept, and the source is scheduled for retry. Apple will isolate only object-level parse/protocol failures; transport/auth/permission failures remain hard failures.

**Tech Stack:** Go 1.25+, PostgreSQL/SQLite event read-model storage, CalDAV/WebDAV, existing `eventsync` coordinator, existing Apple recurrence parser.

**Base revision:** `58dcadc50fad4ec3983c8cb234b0d11dbef25107` (`fix(calendar): tolerate Apple path-only inventory`).

---

## Invariants to preserve

- Providers remain authoritative; this change is read-only at the provider.
- A malformed object must not make valid sibling objects disappear from the cache.
- A degraded terminal result must never advance the provider cursor or perform a full-sync generation sweep.
- A successful empty object remains deletable only when the object was parsed successfully and is known to contain no events in the materialized window.
- Transport, authentication, permission, rate-limit, lease-loss, and cancellation failures remain hard failures with the existing retry/park policy.
- No opaque cursor, page token, provider response body, credential, or full object payload is added to logs or API responses.
- Existing direct-provider MCP and REST reads remain unchanged.

## Model-led execution strategy

Minimum Sol usage is a hard planning constraint. At least 90% of delegable implementation and focused verification is assigned to `gpt-5.6-terra`/`gpt-5.6-luna`; `gpt-5.6-sol` owns no implementation lane. Sol defaults to `medium` reasoning for coordination/review and escalates to `high` only for a named unresolved cross-cutting risk. Percentages are work-allocation targets, not guaranteed token savings.

| Workstream | Primary model | Effort | Owned files | Required checks | Coordinator gate |
|---|---|---:|---|---|---|
| Sync contract and coordinator degraded semantics | `gpt-5.6-terra` | high | `internal/calendar/event_sync.go`, `internal/eventsync/service.go`, related tests | focused Go tests | `gpt-5.6-sol/medium` only if cursor/status invariants remain ambiguous; `high` only if medium cannot resolve it |
| Apple per-object isolation and fixtures | `gpt-5.6-terra` | high | `internal/apple/event_sync.go`, `internal/apple/event_sync_test.go` | Apple unit tests and malformed-object fixtures | review transport-vs-parse failure boundary |
| Storage degraded commit behavior | `gpt-5.6-terra` | high | `internal/storage/event_cache.go`, `internal/storage/event_cache_test.go` | storage tests, transaction assertions | review no-cursor/no-sweep behavior |
| Production-shaped integration/parity fixture | `gpt-5.6-luna` | medium | `internal/storage/postgres_integration_test.go`, test fixtures only | opt-in Postgres test | Terra reviews evidence; no production access |
| Documentation and plan update | `gpt-5.6-luna` | low | `docs/ai-plans/2026-08-22-apple-read-model-recovery.html`, issue links | markdown/HTML checks | coordinator checks factual accuracy |
| Cross-package integration and release decision | `gpt-5.6-terra` | high | no new ownership; inspect all diffs | `go test ./...`, `go vet ./...`, race suite | `gpt-5.6-sol/medium` only for explicit release gate; `high` only for a named unresolved risk |

### Execution waves

1. **Contract freeze — Terra high:** define the degraded page/result fields and invariants; no delegated implementation starts before the interface is fixed.
2. **Parallel implementation — at most two Terra lanes plus Luna:** after the contract lane freezes shared types, run Apple and storage lanes with disjoint ownership. Luna prepares the opt-in integration fixture and documentation; the four-slot limit includes the root coordinator.
3. **Sequential handoff — Luna then Terra:** Luna finishes the synthetic fixture/documentation lane first; only its artifact and checks unlock Terra-high cross-package integration. Do not launch a second Terra integration reviewer in parallel.
4. **Integration — `gpt-5.6-terra` high:** inspect actual diffs, run focused tests, then the full suite and race checks. Two correction turns are the default maximum per lane; reassess the decomposition after that.
5. **Production gate — `gpt-5.6-sol/medium` only when explicitly authorized:** keep flags disabled during implementation; commit, push, migrate, enable, or deploy only after separate user approval and fresh immutable-image verification. Escalate Sol to `high` only for a named unresolved production risk.

Every delegated brief must use `fork_turns="none"`, include this base SHA, list allowed and forbidden files, prohibit commit/push/deploy/secrets, and return changed files, exact checks, limitations, and unresolved risks.

## Current behavior and failure

- `internal/apple/event_sync.go` currently returns a protocol error from `appleSyncPage`/`appleReplacementPage` when `appleSyncMembers` fails for one fetched object.
- `internal/eventsync/service.go` parks protocol failures and does not apply that page; it currently has no attempt-level degraded accumulator across pages.
- `internal/storage/event_cache.go` only sweeps old generations and advances the cursor on a successful final page, but it has no recoverable degraded state that can preserve valid partial results and be claimed again when due.
- Production evidence: the direct Apple path returned 2 valid events while the cache remained empty and the source was parked with `protocol`.

## Desired behavior

For a page containing valid objects plus one malformed object:

1. Parse and materialize valid objects.
2. Record a safe warning code and mark the page degraded.
3. Apply valid upserts/object membership in one transaction.
4. Do not advance `calendar_sync_state.cursor`, even if the malformed object occurred on an earlier non-terminal page and a later page is clean.
5. Do not delete older generations or old membership belonging to unresolved objects.
6. Set `calendar_sync_state.status=degraded`, persist a safe `protocol` error code, release the lease, and schedule a retry using the existing one-minute Apple retry base (with explicit canary monitoring because it is not exponential).
7. Retry from the unchanged cursor; initial replacement retries from an empty cursor and incremental sync retries the same delta.

## File map

**Modify:**

- `internal/calendar/event_sync.go` — add provider-neutral per-page warning metadata to `EventSyncPage`.
- `internal/eventsync/service.go` — validate and map degraded terminal pages; pass status/error metadata to storage without treating them as successful cursor advancement.
- `internal/storage/models.go` — extend `EventSyncBatch` with degraded/error fields (contract ownership is frozen before the storage lane starts).
- `internal/storage/event_cache.go` — atomically apply valid rows while preserving cursor/generation and setting degraded status.
- `internal/apple/event_sync.go` — isolate parse/protocol errors per object; retain hard-failure behavior for fetch/auth/transport errors.
- `internal/apple/event_sync_test.go` — malformed-object and mixed-validity coverage.
- `internal/eventsync/service_test.go` — terminal degraded page contract coverage.
- `internal/storage/event_cache_test.go` — cursor, generation, status, and old-row preservation coverage.
- `internal/storage/postgres_integration_test.go` — opt-in production-shaped parity fixture if the existing harness supports it.
- `internal/web/api.go` and its tests — expose the existing safe source status as `failed/protocol` while allowing partial cached events; no new public `degraded` enum is added in this plan.
- `docs/ai-plans/2026-08-22-apple-read-model-recovery.html` — update with actual implementation results after approval.

**Do not modify:** provider write paths, direct `ListEventsV2`, MCP/REST contracts, database migrations, frontend behavior, deployment defaults, or secrets.

## Task 1: Freeze the degraded terminal contract

**Execution owner:** Terra, high effort. **Coordinator gate:** contract tests must establish that degraded terminal pages apply data but do not advance the cursor.

**Files:** `internal/calendar/event_sync.go`, `internal/storage/models.go`, `internal/eventsync/service.go`, their tests.

- [ ] Add explicit fields with safe semantics:

```go
type EventSyncWarning struct {
    Code     EventSyncErrorClass
    ObjectID string
}

type EventSyncPage struct {
    // existing fields remain unchanged
    Complete bool
    Warnings []EventSyncWarning
}

type EventSyncBatch struct {
    // existing fields remain unchanged
    Degraded  bool
    ErrorCode string
}
```

Warnings are allowed on every page and are accumulated by `eventsync.RunOne` in an `attemptDegraded` flag. Page-level degradation is derived exclusively from whether warnings were returned; there is no separate provider-controlled page `Degraded` boolean. A page-level warning does not by itself complete or fail the attempt. The terminal storage batch is marked degraded if any page warned, and only a non-degraded attempt may carry or persist a new cursor. The public API continues to expose `failed` with safe `protocol` status while cached valid events remain queryable.

- [ ] Extend `validatePage` so warning codes are exactly the recoverable set (`protocol`) and warning object IDs are non-empty. Non-terminal pages may carry warnings; the coordinator, not the provider page, determines whether the terminal attempt is degraded.
- [ ] Accumulate warnings across all pages in `RunOne`; a clean terminal page after an earlier warning must still produce a degraded final batch with no cursor.
- [ ] Map warnings into `EventSyncBatch` without copying provider response text. Use `protocol` as the only persisted recoverable error code.
- [ ] Add service tests for: warning on a non-terminal page followed by a clean terminal page; warning on the terminal page; invalid warning/cursor combination; and unchanged cursor after degraded application.

Run:

```bash
GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/eventsync ./internal/calendar -count=1
```

Expected: PASS after the contract and coordinator changes.

## Task 2: Add an atomic degraded storage commit

**Execution owner:** Terra, high effort. **Coordinator gate:** inspect the transaction to prove no cursor advancement and no generation sweep on degraded final pages.

**Files:** `internal/storage/event_cache.go`, `internal/storage/event_cache_test.go`.

- [ ] Extend `ApplyEventSyncPage` so a final degraded batch applies valid rows, sets `status='degraded'`, stores the safe `protocol` error code, schedules retry, and releases the lease, while leaving cursor and generation authoritative.
- [ ] Update `ClaimDueCalendarSync` so `degraded` is claimable only when `next_sync_at <= now`; add tests for not-due and due degraded states. The state must not become a dead terminal state.
- [ ] Ensure `batch.FullSync && batch.Degraded` never executes either generation sweep.
- [ ] Ensure malformed objects are absent from `ReplacedObjectIDs`, `DeletedObjectIDs`, and `Inventory`; their previous membership remains untouched.
- [ ] Add tests that seed a previous ready snapshot, apply a degraded replacement with one valid and one unresolved object, and assert valid event presence, unresolved old-row preservation, unchanged cursor, unchanged generation, unchanged unresolved-object inventory generation, unchanged `last_success_at`, no sweep, degraded status, protocol error, released lease, and scheduled retry.
- [ ] Add a stale-worker test where a reclaimed lease attempts a degraded batch containing mutations and receives `ErrCalendarSyncLeaseLost` without changing projection or sync state.

Run:

```bash
GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/storage -count=1
```

Expected: PASS for SQLite; the existing opt-in Postgres harness covers locking when `TEST_POSTGRES_URL` is present.

## Task 3: Isolate Apple parse failures per resource

**Execution owner:** Terra, high effort. **Coordinator gate:** transport/auth/permission failures remain hard failures; only object-level parser failures become degraded warnings.

**Files:** `internal/apple/event_sync.go`, `internal/apple/event_sync_test.go`.

- [ ] Introduce a read-model-specific raw object fetch/classification boundary so HTTP status and transport errors are classified before iCalendar body decoding. `401`, `403`, `429` (including `Retry-After`), `5xx`, network errors, and cancellation remain hard failures; syntactically malformed ICS and semantically invalid recurrence/time data become object-level `protocol` warnings.
- [ ] Keep malformed WebDAV inventory and path canonicalization errors fail-fast; they are not object-level warnings.
- [ ] In `appleSyncPage` and `appleReplacementPage`, continue after a classified object parser failure and append a safe `protocol` warning for that object.
- [ ] Do not add the malformed object to `Inventory`, `ReplacedObjectIDs`, or `DeletedObjectIDs`; this preserves prior membership and makes the unchanged cursor retry it.
- [ ] Preserve current behavior for successfully parsed empty objects: emit `DeletedObjectIDs`. Treat an object containing recurrence exceptions but no master VEVENT as malformed/degraded, not as an authoritative empty object.
- [ ] Allow page warnings on intermediate pages; set `NextCursor` only when the entire attempt has no warnings. A later clean page cannot erase an earlier warning.
- [ ] Add tests for two valid resources plus one malformed resource, all resources malformed, no-master recurrence exceptions, 401/403/429/5xx/network/cancellation classification, and the existing empty-token/calendar-query fallback.

Run:

```bash
GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/apple -count=1
```

Expected: PASS with existing Apple Family Sharing/path-only inventory coverage.

## Task 4: Verify end-to-end parity and rollout safety

**Execution owner:** `gpt-5.6-luna`, medium effort for fixtures, then `gpt-5.6-terra`, high effort for integration. **Coordinator gate:** no production enablement until parity evidence is reviewed.

**Files:** existing Apple/storage integration tests and the HTML execution record.

- [ ] Add an opt-in Postgres fixture with two valid resources and one malformed resource; use synthetic payloads only.
- [ ] Compare direct provider and read-model event IDs for the same fixture/window. The API exposes `failed` with safe `protocol` status, cached valid events remain available, and the source must not appear fully fresh/ready.
- [ ] Repair the malformed fixture, retry from the unchanged cursor, and assert transition to `ready`, a successful generation sweep, and inclusion of the repaired resource. Advance the cursor only when the provider returns one; the Apple cursorless fallback remains ready with an intentionally empty cursor.
- [ ] Run:

```bash
GOCACHE=/private/tmp/calendar-mcp-go-build go test ./internal/apple ./internal/eventsync ./internal/storage -count=1
GOCACHE=/private/tmp/calendar-mcp-go-build go test -race ./... -count=1
go vet ./...
go test -tags=integration ./internal/storage -count=1
```

Expected: all commands pass. A missing `TEST_POSTGRES_URL` is a reported limitation, not production evidence.

- [ ] Keep `EVENT_READ_MODEL_ENABLED=false` during implementation. Both `calendar-serve` and `calendar-worker` consume this flag; there is no separate worker flag. After separate deployment approval, warm up one canary source, verify direct/cache parity and API status, monitor repeated Apple fetch volume, then enable reads only if no source is empty without an explicit `failed/protocol` explanation.

## Risks and mitigations

- **Stale events remain for unresolved objects.** Retain them but expose degraded status, keep `last_success_at` unchanged, and retry from the unchanged cursor.
- **A parser bug is misclassified as recoverable.** Only object parser errors become warnings; HTTP/auth/permission/lease errors stay fatal.
- **A degraded page accidentally sweeps data.** Enforce `FullSync && Degraded` no-sweep behavior with transaction tests.
- **Repeated malformed resources cause repeated downloads.** The current Apple policy retries from a one-minute base without exponential growth; monitor request volume during canary and do not enable broad rollout until it is acceptable.
- **Direct/read-model semantics diverge.** Require a production-shaped parity fixture before canary.
- **Concurrent retries race.** Preserve existing lease/CAS checks; stale workers must receive `ErrCalendarSyncLeaseLost` and perform no second mutation.

## Rollback plan

1. Set `EVENT_READ_MODEL_ENABLED=false` for both `calendar-serve` and `calendar-worker`.
2. Stop `calendar-worker` if provider load or retry volume is abnormal.
3. Leave additive schema and rows intact; do not drop tables or reset provider cursors.
4. Redeploy the previous immutable image only with explicit approval.
5. Keep direct provider reads as the active browser path.

## Open questions

- Initially expose the existing generic `failed` + `protocol` source status; do not add or expose Apple object paths.
- Keep the existing one-minute Apple retry base initially; tune after canary request-volume evidence.
- No webhook or migration work is required for this fix.

## Execution record — 2026-08-22

- Contract lane implemented by `gpt-5.6-terra/high`: per-page protocol warnings, attempt-wide degradation, and cursor suppression.
- Storage lane implemented by `gpt-5.6-terra/high`: atomic degraded commit, due-state retry, no-sweep/no-cursor guarantees, and lease regression tests.
- Apple lane implemented by `gpt-5.6-terra/high`: typed raw fetch classification, per-resource malformed-object isolation, orphan recurrence protection, and fallback coverage.
- Parity fixture implemented by `gpt-5.6-luna/medium`; final suite review performed by `gpt-5.6-terra/high`.
- `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./... -count=1` — passed.
- `GOCACHE=/private/tmp/calendar-mcp-go-build go test -race ./... -count=1` — passed.
- `GOCACHE=/private/tmp/calendar-mcp-go-build go vet ./...` — passed.
- `go test -tags=integration ./internal/storage -count=1` — passed with PostgreSQL tests skipped because `TEST_POSTGRES_URL` is unset.
- The Apple suite required approved local-listener escalation in the sandbox; it passed after escalation.
- Sol `medium` re-review initially found two blockers; both were fixed: invalid/missing per-object `Content-Type` is now a hard protocol failure, and degraded terminal retries use the bounded provider retry base. The follow-up Sol review approved with no residual blocker.
- No commit, push, production migration, feature-flag enablement, or deployment was performed. The only unverified layer is execution against live PostgreSQL.

## Approval gate

Implementation must not start until the user explicitly approves this plan. Approval covers multi-page warning accumulation, degraded retry lifecycle, the existing `failed/protocol` API behavior, typed Apple error classification, no-master safety, tests, and staged canary design. It does not authorize commit, push, feature-flag enablement, migration, or deployment.
