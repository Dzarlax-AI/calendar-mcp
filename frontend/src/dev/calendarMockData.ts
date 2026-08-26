export type CalendarMockState = "populated" | "pending" | "degraded" | "empty" | "error" | "no-writable";

type JsonObject = Record<string, unknown>;
type SearchParams = { get(name: string): string | null };

export type CalendarMockRequest = {
  method: string;
  pathname: string;
  searchParams: SearchParams;
  body?: JsonObject;
};

export type CalendarMockResponse = { status: number; body?: unknown };

const csrfToken = "local-calendar-mock-csrf-token";

// These intentionally mirror the raw /api/ui contract rather than the UI's
// normalized models, so the local workbench exercises the same boundary.
export const calendarMockFixture = {
  bootstrap: {
    csrf_token: csrfToken,
    username: "Local mock data",
    calendars: [
      { id: "google:primary", name: "Work and a deliberately long calendar name", provider: "google", connection_name: "Google Workspace", can_read: true, can_write: true, supports_recurrence: true, color: "#4762ee" },
      { id: "microsoft:team", name: "Product team", provider: "microsoft", connection_name: "Microsoft 365", can_read: true, can_write: true, supports_recurrence: true, color: "#2d9b5c" },
      { id: "apple:family", name: "Family", provider: "apple", connection_name: "iCloud", can_read: true, can_write: false, supports_recurrence: false, color: "#f39a2f" },
      { id: "google:travel", name: "Travel and time zones", provider: "google", connection_name: "Google Workspace", can_read: true, can_write: true, supports_recurrence: false, color: "#8959d9" },
      { id: "microsoft:focus", name: "Focus time", provider: "microsoft", connection_name: "Microsoft 365", can_read: true, can_write: true, supports_recurrence: false, color: "#d64367" },
      { id: "apple:birthdays", name: "Birthdays", provider: "apple", connection_name: "iCloud", can_read: true, can_write: false, supports_recurrence: false, color: "#2b9bb1" },
      { id: "google:unavailable", name: "Imported calendar with a very long disabled label", provider: "google", connection_name: "Google Workspace", can_read: false, can_write: false, supports_recurrence: false, color: "#697386" },
    ],
    capabilities: {
      "google:primary": { operations: { list: true, get: true, create: true, update: true, delete: true }, mutation_scopes: ["single", "following", "series"] },
      "microsoft:team": { operations: { list: true, get: true, create: true, update: true, delete: true }, mutation_scopes: ["single", "following", "series"] },
      "apple:family": { read_only: true, operations: { list: true, get: true, create: false, update: false, delete: false } },
      "google:travel": { operations: { list: true, get: true, create: true, update: true, delete: true } },
      "microsoft:focus": { operations: { list: true, get: true, create: true, update: true, delete: true } },
      "apple:birthdays": { read_only: true, operations: { list: true, get: true, create: false, update: false, delete: false } },
      "google:unavailable": { read_only: true, operations: { list: false, get: false, create: false, update: false, delete: false } },
    },
    connections: [],
    rules: [],
    runs: [],
    settings: { timezone: "Europe/Belgrade", mcp_endpoint: "https://example.invalid/mcp" },
  },
  events: [
    { id: "all-day", calendar_id: "google:primary", provider: "google", title: "All-day launch preparation", start: { date: "2026-08-24" }, end: { date: "2026-08-25" }, etag: "all-day-v1" },
    { id: "all-day-two", calendar_id: "microsoft:team", provider: "microsoft", title: "All-day stakeholder updates", start: { date: "2026-08-25" }, end: { date: "2026-08-26" }, etag: "all-day-v2" },
    { id: "all-day-three", calendar_id: "google:travel", provider: "google", title: "All-day travel hold", start: { date: "2026-08-25" }, end: { date: "2026-08-26" }, etag: "all-day-v3" },
    { id: "all-day-four", calendar_id: "microsoft:focus", provider: "microsoft", title: "All-day focus block", start: { date: "2026-08-25" }, end: { date: "2026-08-26" }, etag: "all-day-v4" },
    { id: "all-day-five", calendar_id: "apple:family", provider: "apple", title: "All-day family reminder", read_only: true, start: { date: "2026-08-25" }, end: { date: "2026-08-26" }, etag: "all-day-v5" },
    { id: "overlap-one", calendar_id: "google:primary", provider: "google", title: "Design review", location: "Room 3A", start: { date_time: "2026-08-25T09:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T10:00:00+02:00", time_zone: "Europe/Belgrade" }, etag: "overlap-one-v1" },
    { id: "overlap-two", calendar_id: "microsoft:team", provider: "microsoft", title: "API contract review", location: "Meet", start: { date_time: "2026-08-25T09:30:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T10:30:00+02:00", time_zone: "Europe/Belgrade" }, etag: "overlap-two-v1" },
    { id: "rich-event", calendar_id: "google:primary", provider: "google", title: "Quarterly planning with a deliberately long title that must remain readable in Event details", location: "A very long location name for the conference room and video bridge", description_format: "html", description: "<p><strong>Agenda:</strong> align the roadmap, dependencies, and owners.</p><p>Bring the <a href='https://example.invalid/brief'>planning brief</a>.</p><script>window.visualAuditUnsafe = true</script>", html_link: "https://calendar.google.com/calendar/event?eid=visual-audit", conference: { solution: "Google Meet", entry_points: [{ type: "video", uri: "https://meet.google.com/visual-audit", label: "Join" }] }, start: { date_time: "2026-08-25T13:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T14:30:00+02:00", time_zone: "Europe/Belgrade" }, etag: "rich-event-v1" },
    { id: "recurring-event", calendar_id: "microsoft:team", provider: "microsoft", title: "Weekly planning", start: { date_time: "2026-08-25T11:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T11:30:00+02:00", time_zone: "Europe/Belgrade" }, recurring_event_id: "weekly-planning-master", original_start: { date_time: "2026-08-25T11:00:00+02:00", time_zone: "Europe/Belgrade" }, etag: "recurring-event-v1" },
    { id: "read-only", calendar_id: "apple:family", provider: "apple", title: "Read-only family event", read_only: true, start: { date_time: "2026-08-28T18:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-28T19:00:00+02:00", time_zone: "Europe/Belgrade" }, etag: "read-only-v1" },
  ],
};

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

function noWritableBootstrap(): JsonObject {
  const bootstrap = clone(calendarMockFixture.bootstrap) as unknown as { calendars: JsonObject[]; capabilities: Record<string, JsonObject> };
  bootstrap.calendars.forEach((calendar) => { calendar.can_write = false; });
  Object.values(bootstrap.capabilities).forEach((capability) => {
    capability.read_only = true;
    const operations = capability.operations as JsonObject;
    operations.create = false;
    operations.update = false;
    operations.delete = false;
  });
  return bootstrap;
}

function sourceResponse(state: CalendarMockState, events: JsonObject[]): CalendarMockResponse {
  if (state === "error") return { status: 503, body: { error: { message: "Mock provider unavailable", code: "provider_unavailable" } } };
  if (state === "empty") return { status: 200, body: { items: [], sources: [], complete: true } };
  if (state === "pending") return { status: 200, body: { items: events, sources: [{ provider: "google", calendar_id: "google:primary", complete: false, status: "pending" }], complete: false } };
  if (state === "degraded") return { status: 200, body: { items: events, sources: [{ provider: "apple", calendar_id: "apple:family", complete: false, status: "degraded", error: "Synthetic provider delay", stale: true, error_code: "provider_unavailable" }], complete: false } };
  return { status: 200, body: { items: events, sources: calendarMockFixture.bootstrap.calendars.filter((calendar) => calendar.can_read).map((calendar) => ({ provider: calendar.provider, calendar_id: calendar.id, complete: true, status: "ready" })), complete: true } };
}

export function createCalendarMockStore(state: CalendarMockState = "populated") {
  let events = clone(calendarMockFixture.events) as unknown as JsonObject[];
  let nextEventNumber = 1;

  return {
    handle(request: CalendarMockRequest): CalendarMockResponse {
      const { method, pathname, searchParams, body = {} } = request;
      if (pathname.endsWith("/bootstrap") && method === "GET") {
        return { status: 200, body: state === "no-writable" ? noWritableBootstrap() : calendarMockFixture.bootstrap };
      }
      if (pathname.endsWith("/events") && method === "GET") return sourceResponse(state, events);
      if (pathname.endsWith("/events") && method === "POST") {
        const calendarId = typeof body.calendar_id === "string" ? body.calendar_id : "google:primary";
        const provider = calendarId.split(":", 1)[0] || "calendar";
        const id = `local-mock-${nextEventNumber++}`;
        const event: JsonObject = {
          id,
          calendar_id: calendarId,
          provider,
          title: typeof body.title === "string" ? body.title : "Untitled event",
          description: typeof body.description === "string" ? body.description : undefined,
          location: typeof body.location === "string" ? body.location : undefined,
          start: body.start,
          end: body.end,
          etag: `${id}-v1`,
        };
        events = [...events, event];
        return { status: 200, body: event };
      }
      if (pathname.endsWith("/event") && method === "GET") {
        const event = findEvent(events, searchParams);
        return event ? { status: 200, body: event } : notFound();
      }
      if (pathname.endsWith("/event") && method === "PATCH") {
        const event = findEvent(events, searchParams);
        if (!event) return notFound();
        ["title", "description", "location", "start", "end", "all_day"].forEach((key) => {
          if (body[key] !== undefined) event[key] = body[key];
        });
        event.etag = `${String(event.id)}-v${Date.now()}`;
        return { status: 200, body: { event } };
      }
      if (pathname.endsWith("/event") && method === "DELETE") {
        const event = findEvent(events, searchParams);
        if (!event) return notFound();
        events = events.filter((candidate) => candidate !== event);
        return { status: 200, body: {} };
      }
      if (/\/calendars\/[^/]+\/refresh$/.test(pathname) && method === "POST") return { status: 204 };
      return { status: 404, body: { error: { message: `Unhandled local mock route: ${pathname}` } } };
    },
  };
}

function findEvent(events: JsonObject[], searchParams: SearchParams): JsonObject | undefined {
  return events.find((event) => event.id === searchParams.get("event_id") && event.calendar_id === searchParams.get("calendar_id"));
}

function notFound(): CalendarMockResponse {
  return { status: 404, body: { error: { message: "Mock event was not found", code: "not_found" } } };
}
