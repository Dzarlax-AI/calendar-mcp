import type { Bootstrap, CalendarRecord, EventCreateRequest, EventListResponse, EventRecord, EventTime, EventUpdateRequest, RawCalendarCapabilities, RunRecord, SyncRule } from "./types";

const API_ROOT = "/api/ui";

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
  }).then((result) => normalizeEvent(result.event ?? result));
}

export function deleteEvent(csrfToken: string, calendarId: string, eventId: string, payload: Pick<EventUpdateRequest, "scope" | "expected_etag" | "effective_from"> = {}): Promise<void> {
  const params = new URLSearchParams({ calendar_id: calendarId, event_id: eventId });
  return request<void>(`/event?${params.toString()}`, {
    method: "DELETE",
    headers: csrfHeaders(csrfToken),
    body: JSON.stringify(payload),
  });
}

type RawCalendar = { id: string; name: string; provider?: string; connection_name?: string; time_zone?: string; timezone?: string; can_read?: boolean; can_write?: boolean; supports_recurrence?: boolean; color?: string };
type RawBootstrap = Omit<Bootstrap, "calendars" | "connections" | "settings" | "rules" | "runs"> & { calendars: RawCalendar[]; capabilities?: Record<string, RawCalendarCapabilities>; connections: Array<{ id: string; provider: string; display_name?: string; label?: string; status: string; connect_url?: string; can_reconnect?: boolean; email?: string }>; rules: RawRule[]; runs: RawRun[]; settings: Bootstrap["settings"] & { mcp_endpoint?: string; legacy_api_key_configured?: boolean } };
type RawRule = { id: string; source_calendar_id?: string; target_calendar_id?: string; state?: string; notification_policy?: string; next_run_at?: string; created_at?: string; updated_at?: string };
type RawRun = { id: string; rule_id?: string; trigger?: string; outcome?: string; started_at?: string; finished_at?: string; error_summary?: string; created_count?: number; updated_count?: number; deleted_count?: number };
type RawEvent = Omit<EventRecord, "start" | "end" | "recurrence" | "calendarId" | "timezone" | "source"> & { calendar_id: string; provider?: string; start: EventTime; end: EventTime; original_start?: EventTime; recurring_event_id?: string; instance_kind?: string; recurrence?: string[]; read_only?: boolean };
type RawEventListResponse = { items: RawEvent[]; sources?: Array<{ provider?: string; calendar_id?: string; complete: boolean; error?: unknown }>; complete: boolean };
type RawOperationResult = RawEvent & { event?: RawEvent };

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
  return { id: raw.id, calendarId: raw.calendar_id, title: raw.title ?? "", description: raw.description, location: raw.location, start: start.value, end: end.value, allDay: start.allDay, timezone: start.timezone, etag: raw.etag, readOnly: raw.read_only, source: raw.provider, originalStart: raw.original_start, recurrence: { isRecurring: Boolean(raw.recurring_event_id || (raw.recurrence?.length ?? 0) > 0), masterId: raw.recurring_event_id, occurrenceStart: raw.original_start?.date_time ?? raw.original_start?.date, scopes: raw.recurring_event_id ? ["single", "following", "series"] : undefined } };
}
function normalizeEventList(raw: RawEventListResponse): EventListResponse {
  return { items: raw.items.map(normalizeEvent), complete: raw.complete, sources: raw.sources?.map((source) => ({ calendar_id: source.calendar_id ?? source.provider ?? "calendar", complete: source.complete, error: source.error ? String(source.error) : undefined })) };
}
