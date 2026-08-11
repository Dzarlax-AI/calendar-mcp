# Unified Calendar Event Lifecycle Design

Status: approved in conversation, pending review of this written specification.

## 1. Objective

Turn `calendar-mcp` into a predictable, safe MCP for event management across Google Calendar, Microsoft 365, and Apple CalDAV. Google Calendar is the first complete reference implementation. The common API exposes portable behavior consistently and reports provider-specific limitations through explicit capabilities and typed errors.

This design also fixes the correctness and reliability findings identified in the project review before adding new event functionality.

## 2. Scope

The implementation covers the complete event lifecycle:

- list, get, and search events;
- create, update, delete, import, move, and respond to events where supported;
- recurring series, expanded instances, exceptions, and cancelled instances;
- series, single-occurrence, and this-and-following mutations;
- all-day and timed events with stable time-zone behavior across DST;
- attendees, organizer data, response state, and guest permissions;
- reminders, visibility, transparency, colors, attachments, and conferencing;
- provider event types, including Google default, birthday, focus-time, out-of-office, working-location, and read-only event types;
- notification control with no notifications as the default;
- optimistic concurrency and machine-readable partial-failure reporting.

The implementation does not include calendar ACLs, settings, free/busy, watch channels or webhooks, or general calendar-management APIs. Those are separate future capabilities, and this design must not prevent adding them later.

## 3. Design Principles

1. **Portable core, explicit extensions.** Common concepts use provider-independent types. Provider-only behavior is represented by typed extensions.
2. **Strict by default.** Unsupported fields or operations return `unsupported_capability`; they are never silently ignored.
3. **Safe writes.** Notifications default to `none`, write operations target an explicit calendar, and recurring mutations require an explicit scope.
4. **No false success.** Provider failures, truncation, and partial results are visible in structured responses.
5. **Backward compatibility.** Existing tools remain available as compatibility wrappers while V2 tools carry the new typed contract.
6. **Provider truthfulness.** Capabilities describe actual behavior per provider and, when necessary, per calendar.

## 4. Architecture

### 4.1 Layers

The server is split into four conceptual layers:

- **Transport:** MCP V1, MCP V2, and REST handlers validate and translate requests.
- **Application:** event use cases, capability validation, pagination, composite operations, notification policy, and structured results.
- **Domain:** provider-independent event, time, patch, recurrence, capability, and error types.
- **Providers:** Google, Microsoft, and Apple adapters that translate the domain contract to native APIs.

The application layer prevents MCP and REST handlers from independently reimplementing parsing and mutation semantics.

### 4.2 Provider Interfaces

The current broad `calendar.Provider` interface is decomposed into a stable core plus optional capability interfaces. Illustrative boundaries are:

- calendar listing and event reading;
- event creation and mutation;
- recurrence instance listing;
- invitation responses;
- move and import operations;
- capability reporting.

The application layer checks capabilities before invoking optional behavior. A provider cannot claim success for a field it did not apply.

### 4.3 Capabilities

`get_calendar_capabilities` returns supported operations and field-level features for a calendar. Capabilities include recurrence scopes, reminders, attachments, conferencing, colors, guest permissions, notification policies, import, move, invitation response, provider event types, and optimistic concurrency.

Capabilities may differ between calendars owned by the user and read-only, delegated, subscribed, or family calendars.

## 5. Domain Model

### 5.1 Event Time

`EventTime` has exactly one representation:

- `date` for an all-day boundary; or
- `date_time` plus an IANA `time_zone` for a timed boundary.

An all-day end remains exclusive. Timed recurring events retain their IANA zone so their wall-clock time remains stable across daylight-saving transitions. Provider adapters must not format a local wall time and label it as UTC.

When a client omits a time zone for a non-recurring timed event, the application resolves the calendar's default time zone. Responses always include the effective zone. Recurring timed events require an explicit or successfully resolved IANA zone before creation.

### 5.2 Event V2

`EventV2` includes:

- provider, calendar ID, event ID, ETag/version, and canonical links when available;
- title, description, location, status, created time, and updated time;
- start, end, original occurrence start, recurring parent ID, and recurrence rules;
- organizer, attendees, response state, and guest permissions;
- reminders, visibility, transparency, color, attachments, and conference data;
- provider event type and typed provider extension;
- capability-relevant read-only flags.

Google-specific data that cannot be represented portably lives in `GoogleEventExtension`. This includes Google extended properties and provider-only event-type properties. Read-only Google fields remain read-only in the domain model.

### 5.3 Presence-Aware Patches

V2 patch DTOs distinguish three states:

- field omitted: preserve current value;
- field present with a value: replace current value;
- nullable or collection field present as empty/null: clear it when the provider permits.

This applies to strings, attendees, reminders, attachments, conference data, recurrence, and provider extensions. It fixes the current inability to clear strings or remove all attendees.

### 5.4 Event References

Provider event references must preserve the provider's actual resource identifier. For Apple, the CalDAV object href/path is the mutation identifier; iCalendar UID remains event metadata. UID must never be assumed to equal the `.ics` filename.

## 6. MCP and REST Contract

### 6.1 V2 Tools

The new provider-independent tool set is:

- `get_calendar_capabilities`;
- `get_events_v2`, with `expanded`, `series`, and `both` recurrence views;
- `get_event_instances`, with a required bounded range and pagination;
- `search_events_v2`;
- `create_event_v2`;
- `update_event_v2` with `series`, `single`, and `following` scopes;
- `delete_event_v2` with the same scopes;
- `respond_to_event_v2`;
- `move_event_v2`;
- `import_event_v2`.

Inputs use typed objects and arrays rather than JSON encoded inside strings. Outputs use structured content and include warnings, pagination metadata, provider metadata, and operation status where relevant.

### 6.2 Legacy Tools

The existing five MCP tools remain available. They delegate to the application layer and retain their current accepted input shapes. They receive correctness fixes, safe notification defaults, pagination, and explicit errors, but new advanced fields are exposed through V2.

Legacy tools are documented as compatibility APIs rather than removed or silently redefined.

### 6.3 REST

REST uses the same application use cases and domain DTOs as MCP. V2 endpoints are versioned under `/api/v2`. Existing `/api` routes remain compatibility wrappers.

## 7. Recurrence Semantics

### 7.1 Reading

The API can return recurring parents, expanded instances, or both. Instance responses retain:

- the recurring parent ID;
- the provider instance ID;
- original occurrence start;
- exception or cancellation status;
- the effective event fields.

Cancelled instances are not discarded when the selected view requires them. Instance listing always uses a bounded time range and supports every provider pagination mechanism.

### 7.2 Mutation Scopes

- `series` mutates the recurring parent.
- `single` mutates or cancels one provider instance and creates an exception where required.
- `following` splits a series at `effective_from`.

Changing between all-day and timed representations requires both boundaries. A partial mutation cannot leave mixed date and date-time boundaries. The resulting start must precede the resulting end after merging the patch with the current event.

### 7.3 This-and-Following

Google does not provide an atomic this-and-following operation. The application therefore implements a recoverable composite operation:

1. fetch and validate the parent series and ETag;
2. validate `effective_from` against the recurrence and produce a preview;
3. create the replacement future series with notifications disabled and an operation marker;
4. constrain the original series using `UNTIL` or a recomputed `COUNT`, guarded by its ETag;
5. return both series IDs and the completed steps.

If step 4 fails, the application attempts to remove the replacement series with notifications disabled. If compensation also fails, the response is `partial_failure` and contains both IDs plus an explicit recovery action. The operation never reports complete success while two overlapping series remain.

Because the operation is not transactionally atomic, clients can request a non-mutating preview before execution. Idempotency markers allow a retry to detect an already-created replacement rather than blindly duplicate it.

## 8. Notifications and Invitations

Every write operation accepts a `notification_policy`:

- `none` (default);
- `external_only` where the provider supports it;
- `all`.

The application validates the requested policy against capabilities. Legacy create, update, and delete operations use `none` unless the caller explicitly opts in through a supported extension. Composite-operation compensation also uses `none`.

Real integration tests must use empty or dedicated test calendars. Tests must not include real attendees, and notifications must be verified as disabled before the first provider write.

## 9. Error and Partial-Result Model

Errors have a stable machine-readable code, human-readable message, provider context, retryability, and relevant identifiers. Required codes include:

- `invalid_argument`;
- `invalid_recurrence`;
- `unsupported_capability`;
- `not_found`;
- `permission_denied`;
- `conflict`;
- `rate_limited`;
- `provider_unavailable`;
- `partial_failure`.

Fan-out reads return per-provider and per-calendar status. A provider outage is distinguishable from a genuinely empty calendar. Apple fallback failures and individual object failures are surfaced instead of converted to empty success.

Retries are bounded and limited to safe reads and explicitly idempotent writes. Retry-After is respected for rate limits. Requests have deadlines, HTTP servers have read/write/idle/header timeouts, and shutdown is graceful.

## 10. Review Findings Included in the Work

The foundational phase addresses all findings from the project review:

- Microsoft date-time conversion to UTC;
- fail-closed authentication unless unauthenticated mode is explicitly enabled;
- Apple href-based mutation identifiers;
- Google and Microsoft pagination;
- presence-aware clearing and Apple attendee updates;
- validation of partial all-day/timed transitions;
- visible fan-out and Apple fallback failures;
- notification safety and consistency;
- server and provider HTTP timeouts;
- token directory creation and surfaced token-persistence failures;
- provider/event prefix validation;
- Docker context hygiene through `.dockerignore`.

## 11. Provider Strategy

### 11.1 Google

Google is the reference implementation for the complete V2 event lifecycle. It supports recurrence and instances, reminders, attachments, Meet, colors, guest permissions, extended properties, notification policies, import, move, invitation responses, and all event types that the Google API permits the authenticated account to read or write.

Provider restrictions remain explicit. For example, read-only event types are returned but rejected for unsupported writes, and immutable event-type transitions are rejected before the request is sent.

### 11.2 Microsoft

Microsoft implements the same portable contract where Graph provides matching behavior. Time-zone conversion, pagination, recurrence identifiers, exceptions, online meetings, invitation responses, and provider notification behavior receive contract tests. Unsupported Google-only fields are rejected through capabilities.

### 11.3 Apple

Apple uses CalDAV resource hrefs for mutations and iCalendar UID for identity metadata. Recurrence and exceptions are translated through iCalendar components where the server supports them. Family Sharing fallbacks retain bounded concurrency but expose incomplete reads and object failures.

## 12. Delivery Phases

1. Stabilize authentication, identifiers, time zones, pagination, updates, errors, notifications, timeouts, and deployment hygiene.
2. Introduce domain V2, application services, capabilities, typed errors, REST V2, and legacy wrappers.
3. Implement Google recurrence reads, series, instances, and single-occurrence mutations.
4. Implement Google event fields, special event types, search, responses, move, import, and conferencing.
5. Implement previewed and recoverable this-and-following mutations.
6. Align Microsoft and Apple with the portable contract and truthful capabilities.
7. Complete documentation, migration notes, test-calendar QA, and rollout checks.

Each phase must be independently buildable and testable. V2 can be disabled during rollback while legacy tools continue operating. No phase removes the existing API.

## 13. Verification Strategy

Automated verification includes:

- unit tests for time zones, DST, all-day boundaries, recurrence parsing, patch presence, ID routing, and error mapping;
- provider contract tests using local fake Google/Graph HTTP servers and a fake CalDAV server;
- pagination, cancellation, retry, rate-limit, timeout, and partial-result tests;
- recurrence matrices for timed and all-day events across DST, including series, single, following, exceptions, and cancelled instances;
- MCP and REST schema tests plus legacy compatibility tests;
- notification-policy tests for create, update, delete, and compensation;
- token-persistence failure tests;
- `go test -race -count=1 ./...`, `go vet ./...`, and a production build.

Manual QA uses dedicated test calendars only. It verifies provider-visible results, recurrence behavior, DST behavior, no-notification defaults, ETag conflicts, permission failures, and recovery from a deliberately interrupted following-series operation.

## 14. Rollout and Rollback

V2 is introduced additively. Deployment can disable V2 routing without removing the legacy tools. Each delivery phase is a natural review and rollback boundary. Provider writes are not enabled in real-calendar QA until notifications and invitations are verified disabled.

Rollback of a code phase uses the previous deploy artifact. Composite operations record enough identifiers in their result and logs for manual recovery; they do not depend on an unavailable database transaction.

## 15. Acceptance Criteria

The design is complete when:

- every review finding has a regression test and a verified fix;
- Google supports the complete event lifecycle described in this document;
- recurrence behavior is correct for series, single occurrences, and following occurrences;
- no write sends notifications unless explicitly requested;
- unsupported provider behavior is rejected rather than ignored;
- paginated or partial provider results cannot appear as complete success;
- existing MCP tools remain callable with their current input shapes;
- automated checks pass and manual QA succeeds on dedicated test calendars.
