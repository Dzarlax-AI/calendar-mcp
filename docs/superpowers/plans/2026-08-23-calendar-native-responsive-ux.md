# Calendar Native Responsive UX Implementation Plan

> **Execution:** Delegated execution is recommended after approval. Minimize Sol usage; implementation belongs to Terra/Luna, with Sol limited to a final high-risk design/integration review. This plan is not authorization to modify code, commit, push, merge, or deploy.

**Goal:** Make the unified calendar feel native and predictable on mobile, tablet, and desktop, while preserving full calendar identities and keeping the primary event action discoverable at every width.

**Architecture:** Keep FullCalendar as the event canvas, but introduce one explicit surface model for filters, event details, create, and recurrence dialogs. Use content-driven responsive layout modes: persistent rail only when the center canvas has enough width; overlay sheets below that threshold. Keep calendar identity as full display name plus provider/account context; never rely on implicit one-letter truncation.

**Tech Stack:** React + TypeScript, FullCalendar, CSS media queries, Vitest, Vite production build, browser-tool manual QA.

**Base revision:** `8b850b9` (`fix/calendar-sync-protocol-isolation`); current local source is the review baseline. Existing unrelated untracked files must remain untouched.

---

## Evidence and confirmed root causes

- The supplied mobile screenshot shows the filters panel covering roughly two thirds of the viewport while the underlying calendar remains visible and apparently actionable.
- A current-source local browser run with synthetic calendars reproduced the one-letter labels. Computed styles showed `.calendar-filter` as `15px 10px 189px 0px` and `.calendar-name` width `10px`.
- The root cause is CSS grid auto-placement: the checkbox input is absolutely positioned, so the name is placed in the second `10px` track. At narrow widths the provider track is hidden, but the name remains trapped in the 10px track.
- The date title renders a down chevron but currently invokes `navigate("today")`; it is not a date picker.
- The mobile sidebar and event drawer have no shared scrim/overlay state, outside-tap close, body lock, or modal semantics.
- The current browser run used synthetic data only. No production credentials or calendar records were accessed.

## Target interaction model

| Width | Composition | Filters | Event details | Primary action |
|---|---|---|---|---|
| `>=1280px` | Persistent left rail + center canvas (`min-width: 680px`) + optional inspector | Rail | Optional right inspector | Toolbar `New event` always visible; sidebar duplicate allowed |
| `821–1279px` | Center-first; no simultaneous fixed side panels | Overlay sheet | Overlay inspector; never simultaneous with filters | Toolbar action always visible |
| `<=820px` | Full-width calendar canvas, compact two-row header | Full-height modal sheet with scrim | Bottom sheet or full-screen modal with scrim | Header action and/or FAB |

One surface state owns mutually exclusive overlays: `"none" | "filters" | "event" | "create" | "scope"`. Escape, outside tap, close button, and successful navigation must close the active surface. Modal surfaces must restore focus to their opener and make the background inert/non-interactive.

## Web-first, native-ready constraints

The web app is the first polished client, not the permanent owner of calendar semantics. The following contracts must remain portable to a future iOS/Android client:

- Keep event/calendar domain types, capability flags, recurrence scopes, error codes, sync status, and mutation outcomes in the API/domain layer; do not encode them only in FullCalendar callbacks or CSS state.
- Keep surface state serializable and platform-neutral. A future native client should be able to reproduce `filters`, `event`, `create`, and `scope` as screens/sheets without depending on browser focus APIs.
- Make view/date/filter state deep-linkable and recoverable: at minimum selected date, view, selected calendar IDs, and event identity should have an explicit persistence/deep-link policy. Do not make localStorage the only source of truth.
- Define time behavior explicitly: account for the user timezone, calendar timezone, DST transitions, all-day exclusive end dates, multi-day events, and recurring-event scope labels. These rules must be shared by web and future native clients.
- Use CSS design tokens and semantic interaction names (`primary action`, `surface`, `warning`, `calendar identity`) rather than styling decisions that cannot be reproduced in native UI.
- Preserve offline/read-model semantics: cached events, stale/degraded/failed source states, retry behavior, and last-success timestamps should remain meaningful without a browser-specific implementation.
- Keep notification/invite safety explicit. CRUD and sync flows must not silently create attendee notifications; any future native notification layer must consume the same server-side outcome and warning contract.

This pass does not build a native app, PWA install flow, push notifications, background refresh, or platform-specific UI. It prevents the web implementation from making those later paths unnecessarily expensive.

## Model-led execution strategy

| Workstream | Primary model | Effort | Owned files | Required checks | Coordinator gate |
|---|---|---:|---|---|---|
| Surface/state contract and React structure | `gpt-5.6-terra` | high | `frontend/src/features/calendar/CalendarPage.tsx`, `frontend/src/lib/types.ts`, focused calendar tests | TypeScript, Vitest, accessibility semantics inspection | Root freezes contract before Luna starts |
| Responsive visual system and CSS mechanics | `gpt-5.6-luna` | medium | `frontend/src/styles/app.css`, CSS-focused fixtures/tests if needed | Frontend tests/build; browser screenshots at required widths | Must implement Terra’s class/state contract without changing behavior |
| Integrated browser QA and reconciliation | Root coordinator | medium | no implementation ownership initially | Go tests, npm tests/build, browser screenshots and DOM/computed-style checks | Accept only after both lanes pass and diffs are inspected |
| Final design/integration review | `gpt-5.6-sol` | medium, review only | no implementation ownership | Review integrated diff + browser evidence + checklist | Sol may request one focused correction turn; no Sol implementation lane |

Allocation target: at least 90% of delegable implementation effort is Terra/Luna; Sol is limited to the final review gate and any named unresolved cross-cutting risk.

## Execution waves

1. **Contract freeze — Terra high.** Define surface state, class names, focus/inert behavior, calendar identity rules, and responsive invariants. Deliver a short contract summary and focused failing tests. No CSS edits.
2. **Parallel implementation — Terra + Luna.** Terra implements React/state/semantics against the frozen contract. Luna implements CSS/layout/touch/safe-area mechanics against the same class names. They must not edit each other’s owned files.
3. **Integration — root coordinator.** Inspect the actual diff, resolve only mechanical conflicts, run tests/build, seed a disposable local SQLite fixture, and capture browser evidence at six viewports with sidebar/drawer open and closed.
4. **Sol gate — Sol medium review only.** Review the integrated diff and screenshots against the target model. If findings remain, permit at most one focused Terra/Luna correction turn; root reruns the complete matrix.
5. **Release decision — root.** This plan does not authorize PR, merge, or deploy. Those require a separate explicit user instruction after the UX review is accepted.

## Task 1: Freeze the surface and identity contract

**Execution owner:** Terra, `gpt-5.6-terra`, high effort.

**Files:**

- Modify: `frontend/src/features/calendar/CalendarPage.tsx`
- Modify: `frontend/src/lib/types.ts`
- Test: `frontend/src/lib/calendar.test.ts`

**Steps:**

- [ ] Add a discriminated surface type and single owner state in `CalendarPage.tsx`:

  ```ts
  type CalendarSurface =
    | { kind: "none" }
    | { kind: "filters" }
    | { kind: "event"; event: EventRecord }
    | { kind: "create"; preset?: Partial<EventDraft> }
    | { kind: "scope"; event: EventRecord; action: "reschedule" | "edit" | "delete" };
  ```

- [ ] Preserve existing mutation semantics while routing open/close actions through that state. Do not introduce a second independent `sidebarOpen`/`selectedEvent` source of truth.
- [ ] Define the rendered calendar identity contract: `CalendarRecord.name` is never shortened in data mapping; the UI may visually clamp only after retaining a full `title` and accessible name. Provider and account label remain available as secondary identity.
- [ ] Make the view switcher expose `aria-pressed={view === option.value}` and persist the selected view under a namespaced local-storage key, with invalid values falling back to the existing responsive default.
- [ ] Replace the date-title fake dropdown behavior with either a real date picker/popover that calls `gotoDate`, or remove the chevron and keep a clearly labeled Today action. The preferred implementation is a native date input/popover with an accessible label and Escape close.
- [ ] Add tests for surface exclusivity, invalid view persistence, full calendar-name preservation, and the existing readable-calendar selection recovery behavior.
- [ ] Record the serialization/deep-link policy for date, view, selected calendars, and selected event in code comments or a small focused design note so a native client can implement the same behavior.

**Forbidden:** CSS edits, provider/API changes, production access, credentials, commits, pushes, merges, or deploys.

**Acceptance:** The contract is explicit enough for Luna to style without guessing; focused frontend tests pass; no behavior change is introduced for create/edit/delete/recurrence payloads.

## Task 2: Implement mobile/tablet/desktop surface behavior

**Execution owner:** Terra, `gpt-5.6-terra`, high effort.

**Files:**

- Modify: `frontend/src/features/calendar/CalendarPage.tsx`
- Modify: `frontend/src/lib/types.ts` only if Task 1 proves a type addition is necessary
- Test: `frontend/src/lib/calendar.test.ts` and any focused component test already supported by the repository

**Steps:**

- [ ] Add a shared overlay primitive for filters and event details with `role="dialog"`, `aria-modal="true"`, a labeled heading, Escape handling, outside-tap close, focus return, and a scrim button that is the only background close target.
- [ ] Ensure only one surface can be open at a time. Opening event details closes filters; opening create closes event details; recurrence scope remains above the underlying event/create flow.
- [ ] Keep `New event` in the main toolbar at every width. The sidebar button may remain as a secondary shortcut when the rail is visible.
- [ ] Add an explicit compact mobile toolbar composition: Today/date title in the first row; view and refresh controls in the second row or an overflow menu. Do not let the four view buttons compete with the title at 390px.
- [ ] Add a real date-picker path or remove the misleading chevron as defined by Task 1.
- [ ] Keep cached events visible when source status is failed/degraded; move status messaging into a reserved strip below the toolbar rather than covering the FullCalendar grid.
- [ ] Separate notification intent in markup (`success`, `info`, `warning`, `error`) and preserve `role="status"` for successful operations and `role="alert"` for actionable failures.
- [ ] Add `aria-current="date"` to the mini-calendar’s current day and synchronize its visible month with the main calendar date range when the user navigates.
- [ ] Verify that timezone, all-day, DST, recurring-scope, stale/degraded, and mutation-warning semantics remain represented in data rather than inferred from FullCalendar presentation.

**Forbidden:** `frontend/src/styles/app.css` edits, backend changes, provider changes, production access, credentials, commits, pushes, merges, or deploys.

**Acceptance:** DOM snapshots show mutually exclusive modal surfaces, accessible close semantics, persistent `New event`, and no fake date-picker affordance; focused tests pass.

## Task 3: Implement the responsive visual system

**Execution owner:** Luna, `gpt-5.6-luna`, medium effort.

**Files:**

- Modify: `frontend/src/styles/app.css`
- Add or modify only CSS fixture/test files if the existing frontend test setup supports them; do not modify React behavior

**Steps:**

- [ ] Fix the calendar identity grid using the proven root cause. The minimum acceptable rule is:

  ```css
  .calendar-filter {
    grid-template-columns: 15px minmax(0, 1fr) auto;
  }
  ```

  Ensure provider/account metadata is not the only visible identity at tablet widths; use a compact provider mark or retained tooltip rather than hiding all context.
- [ ] Style the shared scrim and sheet states from Task 2. Below 820px, filters are `width: min(100%, 360px)` or full-width on narrow phones; event details become a bottom sheet/full-screen surface. At 821–1279px, overlays must not reduce the center canvas width.
- [ ] Define responsive layout modes around the target breakpoints and preserve a center canvas minimum width. Do not keep the current fixed `218px + center + 300px` composition through tablet widths.
- [ ] Keep toolbar primary action visible and prevent overflow at 390px with a two-row layout or explicit horizontal scroll/overflow menu.
- [ ] Increase interactive hit areas to at least 44px without enlarging icons unnecessarily: mini-calendar buttons, calendar filter rows, icon buttons, and mobile toolbar buttons.
- [ ] Add `100dvh` fallback behavior and `env(safe-area-inset-top/right/bottom/left)` where sheets and bottom actions touch device edges.
- [ ] Add visible focus rings for the entire calendar filter row, not just the 14px color swatch.
- [ ] Add visual variants for success/info/warning/error notices and the inline sync-status strip.

**Forbidden:** changes to TypeScript/React behavior, API payloads, provider code, production access, credentials, commits, pushes, merges, or deploys.

**Acceptance:** `npm test -- --run` and `npm run build` pass; CSS has no 10px name track; the layout has no horizontal overflow at 390px, 430px, 768px, 1024px, 1280px, or 1440px.

## Task 4: Build a disposable browser QA fixture

**Execution owner:** Root coordinator, inline/local tooling only.

**Files:**

- No production files required.
- Temporary fixture only under `/private/tmp` or a disposable SQLite database; never under tracked source.

**Steps:**

- [ ] Use SQLite with three synthetic connected accounts and six calendars whose names are deliberately long: `Personal & Family`, `Focus Time / Deep Work`, `Family Shared Calendar`, `Birthdays and Anniversaries`, `Work Projects and Meetings`, and `Travel and Reservations`.
- [ ] Seed at least three events with title, time, location, read-only state, and different provider colors so the event drawer and identity line are inspectable.
- [ ] Capture browser screenshots and DOM snapshots at `390x844`, `430x932`, `768x1024`, `1024x768`, `1280x800`, and `1440x900`, each with filters closed/open and event details closed/open where the layout permits.
- [ ] Check computed `.calendar-name` widths, toolbar overflow, `aria-modal`, `aria-pressed`, focus return, Escape, outside-tap behavior, body scroll lock, `100dvh`, and safe-area padding.
- [ ] Verify mobile filters and event details are never simultaneously open below desktop mode.

**Acceptance:** Every target viewport has an accepted screenshot; the evidence includes full calendar names and at least one event detail surface; limitations are recorded when a state cannot be reached without provider credentials.

## Task 5: Integrated verification and accessibility pass

**Execution owner:** Root coordinator, medium effort.

**Files:** No new production ownership; inspect all changes.

**Checks:**

- [ ] `GOCACHE=/private/tmp/calendar-mcp-go-build go test ./...`
- [ ] `GOCACHE=/private/tmp/calendar-mcp-go-build go vet ./...`
- [ ] `git diff --check`
- [ ] `cd frontend && npm test -- --run`
- [ ] `cd frontend && npm run build`
- [ ] Browser manual matrix from Task 4.
- [ ] Domain compatibility checks: timezone/DST and all-day exclusive-end cases; recurring single/following/series labels; stale/degraded/failed source states; conflict/permission/rate-limit error codes; and notification-suppression warnings.
- [ ] Keyboard matrix: Tab into filters, open/close filters, open event, edit, cancel, Escape, and return focus to opener.
- [ ] Mutation regression checks: create, edit, delete, recurring scope, conflict, permission failure, and notification semantics remain unchanged.
- [ ] Local-storage checks: empty, stale, unreadable, and corrupt `calendar:selected`/view values recover safely.

## Task 6: Sol review gate

**Execution owner:** `gpt-5.6-sol`, medium effort, review only.

Sol receives the integrated diff, the six viewport screenshots, DOM/computed-style evidence, and test output. Sol must review:

- surface exclusivity and native mobile sheet behavior;
- center-canvas minimum width and breakpoint correctness;
- full calendar identity and provider disambiguation;
- touch targets, safe areas, focus, Escape, inert background, and ARIA;
- whether the date affordance is honest;
- regression risk to existing calendar CRUD and sync-status behavior.

Sol may return `approved`, `approved with non-blocking notes`, or `changes requested` with file-specific findings. Allow at most one focused correction turn before root re-runs Task 5; escalate further only for a named unresolved architecture or accessibility risk.

## Risks and mitigations

- **Overlay regressions:** shared surface state can accidentally close or overwrite mutation dialogs. Mitigate with state-transition tests and keyboard QA.
- **FullCalendar resize bugs:** pane changes can leave stale dimensions. Call `calendarRef.current?.getApi().updateSize()` after layout mode changes and verify screenshots.
- **Identity ambiguity:** provider labels may be hidden at exactly the widths where names are most constrained. Keep full name accessible and show provider mark/account label.
- **Safari viewport behavior:** `100vh` and bottom actions can be clipped by browser chrome/home indicator. Test dynamic toolbar, rotation, and safe-area padding.
- **User preference loss:** migrating sidebar/view storage can reset selections. Read old keys, validate values, and fall back without throwing.
- **Scope creep:** search, swipe gestures, resizable panes, density settings, and bottom navigation are explicitly deferred to a later plan.
- **Future native divergence:** if date/filter/surface state is only stored in browser-specific controls, a native client will need to reverse-engineer behavior. Keep the state and outcome contracts explicit now.

## Rollback plan

Revert the UI commits as one bounded change set. No database migration, provider credential, or production configuration change is required. The previous FullCalendar layout and independent surface behavior remain a functional fallback. Before any release action, compare the integrated diff against base `8b850b9` and rerun the full verification matrix.

## Approval gate

Implementation must not start until the user explicitly approves this plan. Approval authorizes the planned local implementation only; PR, merge, deploy, and external notifications remain separate explicit actions.
