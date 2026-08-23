import type { Bootstrap, CalendarRecord, EventCreateRequest, EventListResponse, EventRecord, EventSourceStatus, EventTime, EventUpdateRequest, RawCalendarCapabilities, RunRecord, SyncRule } from "./types";

const API_ROOT = "/api/ui";
const MAX_BROWSER_WARNINGS = 3;

export class APIError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(message: string, status: number, code?: string) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.code = code;
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  const response = await fetch(`${API_ROOT}${path}`, { ...init, headers, credentials: "same-origin" });
  const raw = await response.text();
  let payload: unknown;
  try {
    payload = raw ? JSON.parse(raw) : undefined;
  } catch {
    payload = undefined;
  }
  if (!response.ok) {
    const body = payload as { message?: string; error?: string | { message?: string; code?: string }; code?: string } | undefined;
    const nested = typeof body?.error === "object" ? body.error : undefined;
    const message = body?.message ?? nested?.message ?? (typeof body?.error === "string" ? body.error : undefined) ?? `Request failed (${response.status})`;
    throw new APIError(message, response.status, body?.code ?? nested?.code);
  }
  return payload as T;
}

export function getBootstrap(): Promise<Bootstrap> {
  return request<RawBootstrap>("/bootstrap").then(normalizeBootstrap);
}

export function getEvents(start: string, end: string, calendarIds: string[]): Promise<EventListResponse> {
  const params = new URLSearchParams({ start, end });
  calendarIds.forEach((id) => params.append("calendar_id", id));
  return request<RawEventListResponse>(`/events?${params.toString()}`).then(normalizeEventList);
}

export function refreshCalendar(csrfToken: string, calendarId: string): Promise<void> {
  return request<void>(`/calendars/${encodeURIComponent(calendarId)}/refresh`, {
    method: "POST",
    headers: csrfHeaders(csrfToken),
  });
}

export function getEvent(calendarId: string, eventId: string): Promise<EventRecord> {
  const params = new URLSearchParams({ calendar_id: calendarId, event_id: eventId });
  return request<RawEvent>(`/event?${params.toString()}`).then(normalizeEvent);
}

function csrfHeaders(csrfToken: string): HeadersInit {
  return { "X-CSRF-Token": csrfToken };
}

export function createEvent(csrfToken: string, payload: EventCreateRequest): Promise<EventRecord> {
  return request<RawEvent>("/events", { method: "POST", headers: csrfHeaders(csrfToken), body: JSON.stringify(payload) }).then(normalizeEvent);
}

export function updateEvent(csrfToken: string, calendarId: string, eventId: string, payload: EventUpdateRequest): Promise<EventRecord> {
  const params = new URLSearchParams({ calendar_id: calendarId, event_id: eventId });
  return request<RawOperationResult>(`/event?${params.toString()}`, {
    method: "PATCH",
    headers: csrfHeaders(csrfToken),
    body: JSON.stringify(payload),
  }).then((result) => {
    const event = normalizeEvent(result.event ?? result.related_events?.[0] ?? result);
    event.warnings = safeWarnings(result.warnings);
    return event;
  });
}

export function deleteEvent(csrfToken: string, calendarId: string, eventId: string, payload: Pick<EventUpdateRequest, "scope" | "expected_etag" | "effective_from"> = {}): Promise<{ warnings?: string[] }> {
  const params = new URLSearchParams({ calendar_id: calendarId, event_id: eventId });
  return request<{ warnings?: unknown }>(`/event?${params.toString()}`, {
    method: "DELETE",
    headers: csrfHeaders(csrfToken),
    body: JSON.stringify(payload),
  }).then((result) => ({ warnings: safeWarnings(result.warnings) }));
}

type RawCalendar = { id: string; name: string; provider?: string; connection_name?: string; time_zone?: string; timezone?: string; can_read?: boolean; can_write?: boolean; supports_recurrence?: boolean; color?: string };
type RawBootstrap = Omit<Bootstrap, "calendars" | "connections" | "settings" | "rules" | "runs"> & { calendars: RawCalendar[]; capabilities?: Record<string, RawCalendarCapabilities>; connections: Array<{ id: string; provider: string; display_name?: string; label?: string; status: string; connect_url?: string; can_reconnect?: boolean; email?: string }>; rules: RawRule[]; runs: RawRun[]; settings: Bootstrap["settings"] & { mcp_endpoint?: string; legacy_api_key_configured?: boolean } };
type RawRule = { id: string; source_calendar_id?: string; target_calendar_id?: string; state?: string; notification_policy?: string; next_run_at?: string; created_at?: string; updated_at?: string };
type RawRun = { id: string; rule_id?: string; trigger?: string; outcome?: string; started_at?: string; finished_at?: string; error_summary?: string; created_count?: number; updated_count?: number; deleted_count?: number };
type RawEvent = Omit<EventRecord, "start" | "end" | "recurrence" | "calendarId" | "timezone" | "source"> & { calendar_id: string; provider?: string; start: EventTime; end: EventTime; original_start?: EventTime; recurring_event_id?: string; instance_kind?: string; recurrence?: string[]; read_only?: boolean; warnings?: unknown };
type RawEventListResponse = { items: RawEvent[]; sources?: RawEventSourceStatus[]; complete: boolean };
type RawEventSourceStatus = { provider?: string; calendar_id?: string; complete: boolean; error?: unknown; status?: string; last_success_at?: string | null; stale?: boolean; error_code?: unknown };
type RawOperationResult = RawEvent & { event?: RawEvent; related_events?: RawEvent[]; warnings?: unknown };

const colors = ["#4762ee", "#2d9b5c", "#f39a2f", "#8959d9", "#d64367", "#2b9bb1"];
function normalizeBootstrap(raw: RawBootstrap): Bootstrap {
  return {
    ...raw,
    calendars: raw.calendars.map((calendar, index): CalendarRecord => {
      const capability = raw.capabilities?.[calendar.id];
      const scopes = capability?.mutation_scopes?.length ? capability.mutation_scopes : calendar.supports_recurrence ? ["single", "following", "series"] as const : ["single"] as const;
      return ({
      id: calendar.id,
      name: calendar.name,
      provider: calendar.provider ?? "calendar",
      accountLabel: calendar.connection_name,
      timezone: calendar.time_zone ?? calendar.timezone,
      color: calendar.color ?? colors[index % colors.length],
      readOnly: capability?.read_only ?? !calendar.can_write,
      capability: { read: capability?.operations?.list ?? calendar.can_read !== false, create: capability?.operations?.create ?? calendar.can_write === true, write: capability?.operations?.update ?? calendar.can_write === true, delete: capability?.operations?.delete ?? calendar.can_write === true, recurring: calendar.supports_recurrence === true, recurrenceScopes: [...scopes] },
    }); }),
    connections: raw.connections.map((connection) => ({ id: connection.id, provider: connection.provider, label: connection.display_name ?? connection.label ?? connection.provider, email: connection.email, status: connection.status, connectURL: connection.connect_url, canReconnect: connection.can_reconnect })),
    rules: raw.rules.map((rule): SyncRule => ({ id: rule.id, sourceCalendarId: rule.source_calendar_id, targetCalendarId: rule.target_calendar_id, state: rule.state, enabled: rule.state === "enabled", lastRun: rule.next_run_at })),
    runs: raw.runs.map((run): RunRecord => ({ id: run.id, ruleId: run.rule_id, trigger: run.trigger, outcome: run.outcome, status: run.outcome ?? "unknown", startedAt: run.started_at, finishedAt: run.finished_at, message: run.error_summary, createdCount: run.created_count, updatedCount: run.updated_count, deletedCount: run.deleted_count })),
    settings: { ...raw.settings, publicURL: raw.settings.mcp_endpoint, mcpEndpoint: raw.settings.mcp_endpoint, legacyApiKeyConfigured: raw.settings.legacy_api_key_configured },
  };
}

function eventTimeValue(time: EventTime): { value: string; allDay: boolean; timezone?: string } {
  return time.date ? { value: time.date, allDay: true } : { value: time.date_time ?? "", allDay: false, timezone: time.time_zone };
}
function normalizeEvent(raw: RawEvent | RawOperationResult): EventRecord {
  const start = eventTimeValue(raw.start);
  const end = eventTimeValue(raw.end);
  return { id: raw.id, calendarId: raw.calendar_id, title: raw.title ?? "", description: raw.description, location: raw.location, start: start.value, end: end.value, allDay: start.allDay, timezone: start.timezone, etag: raw.etag, readOnly: raw.read_only, source: raw.provider, originalStart: raw.original_start, warnings: safeWarnings(raw.warnings), recurrence: { isRecurring: Boolean(raw.recurring_event_id || (raw.recurrence?.length ?? 0) > 0), masterId: raw.recurring_event_id, occurrenceStart: raw.original_start?.date_time ?? raw.original_start?.date, scopes: raw.recurring_event_id ? ["single", "following", "series"] : undefined } };
}

function safeWarnings(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const warnings = value.filter((item): item is string => typeof item === "string" && item.length > 0 && item.length <= 200).slice(0, MAX_BROWSER_WARNINGS);
  return warnings.length ? warnings : undefined;
}
function normalizeEventList(raw: RawEventListResponse): EventListResponse {
  return { items: raw.items.map(normalizeEvent), complete: raw.complete, sources: raw.sources?.map(normalizeSourceStatus) };
}

const SAFE_ERROR_CODES = new Set(["invalid_argument", "invalid_recurrence", "unsupported_capability", "not_found", "permission_denied", "conflict", "rate_limited", "provider_unavailable", "partial_failure"]);
const SAFE_STATUS = new Set(["pending", "syncing", "ready", "failed", "parked", "degraded"]);

function safeErrorCode(value: unknown): string | null {
  if (typeof value !== "string") return null;
  const code = value.trim();
  return /^[a-z][a-z0-9_]{0,63}$/.test(code) ? code : null;
}

function sourceErrorMessage(code: string | null): string | undefined {
  if (!code) return undefined;
  switch (code) {
    case "permission_denied": return "Calendar access was denied";
    case "rate_limited": return "Calendar provider is rate limited";
    case "not_found": return "Calendar was not found";
    case "unsupported_capability": return "This calendar operation is not supported";
    case "invalid_argument":
    case "invalid_recurrence": return "Calendar request is invalid";
    case "conflict": return "The calendar changed elsewhere";
    case "partial_failure": return "Some calendar sources could not be loaded";
    case "provider_unavailable": return "Calendar provider is temporarily unavailable";
    default: return SAFE_ERROR_CODES.has(code) ? "Calendar provider could not be reached" : "Calendar provider is temporarily unavailable";
  }
}

function normalizeSourceStatus(source: RawEventSourceStatus): EventSourceStatus {
  const errorCode = safeErrorCode(source.error_code);
  const status = typeof source.status === "string" && SAFE_STATUS.has(source.status) ? source.status : source.complete && !source.error ? "ready" : "failed";
  return {
    provider: source.provider ?? "calendar",
    calendar_id: source.calendar_id ?? source.provider ?? "calendar",
    complete: source.complete,
    status,
    last_success_at: source.last_success_at ?? null,
    stale: source.stale === true,
    error_code: errorCode,
    ...(source.error || errorCode ? { error: sourceErrorMessage(errorCode) ?? "Calendar provider is temporarily unavailable" } : {}),
  };
}
