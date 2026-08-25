import type { Page } from "@playwright/test";

type FixtureState = "populated" | "pending" | "degraded" | "empty" | "error" | "no-writable";

const csrfToken = "visual-audit-csrf-token";

// These are deliberately raw /api/ui payloads. Keeping the fixture on the wire
// contract exercises the browser normalizers as well as the Calendar surface.
export const calendarFixture = {
  bootstrap: {
    csrf_token: csrfToken,
    username: "Visual audit",
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
    { id: "overlap-one", calendar_id: "google:primary", provider: "google", title: "Design review", location: "Room 3A", start: { date_time: "2026-08-25T09:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T10:00:00+02:00", time_zone: "Europe/Belgrade" }, etag: "overlap-one-v1" },
    { id: "overlap-two", calendar_id: "microsoft:team", provider: "microsoft", title: "API contract review", location: "Meet", start: { date_time: "2026-08-25T09:30:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T10:30:00+02:00", time_zone: "Europe/Belgrade" }, etag: "overlap-two-v1" },
    { id: "rich-event", calendar_id: "google:primary", provider: "google", title: "Quarterly planning with a deliberately long title that must remain readable in Event details", location: "A very long location name for the conference room and video bridge", description_format: "html", description: "<p><strong>Agenda:</strong> align the roadmap, dependencies, and owners.</p><p>Bring the <a href='https://example.invalid/brief'>planning brief</a>.</p><script>window.visualAuditUnsafe = true</script>", html_link: "https://calendar.google.com/calendar/event?eid=visual-audit", conference: { solution: "Google Meet", entry_points: [{ type: "video", uri: "https://meet.google.com/visual-audit", label: "Join" }] }, start: { date_time: "2026-08-25T13:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T14:30:00+02:00", time_zone: "Europe/Belgrade" }, etag: "rich-event-v1" },
    { id: "recurring-event", calendar_id: "microsoft:team", provider: "microsoft", title: "Weekly planning", start: { date_time: "2026-08-25T11:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-25T11:30:00+02:00", time_zone: "Europe/Belgrade" }, recurring_event_id: "weekly-planning-master", original_start: { date_time: "2026-08-25T11:00:00+02:00", time_zone: "Europe/Belgrade" }, etag: "recurring-event-v1" },
    { id: "read-only", calendar_id: "apple:family", provider: "apple", title: "Read-only family event", read_only: true, start: { date_time: "2026-08-28T18:00:00+02:00", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-08-28T19:00:00+02:00", time_zone: "Europe/Belgrade" }, etag: "read-only-v1" },
  ],
} as const;

export type CalendarFixtureOptions = { state?: FixtureState };

function responseForState(state: FixtureState) {
  if (state === "error") return { status: 503, body: { error: { message: "Fixture provider unavailable", code: "provider_unavailable" } } };
  if (state === "empty") return { status: 200, body: { items: [], sources: [], complete: true } };
  if (state === "pending") return { status: 200, body: { items: calendarFixture.events, sources: [{ provider: "google", calendar_id: "google:primary", complete: false, status: "pending" }], complete: false } };
  if (state === "degraded") return { status: 200, body: { items: calendarFixture.events, sources: [{ provider: "apple", calendar_id: "apple:family", complete: false, status: "degraded", error: "Synthetic provider delay", stale: true, error_code: "provider_unavailable" }], complete: false } };
  return { status: 200, body: { items: calendarFixture.events, sources: calendarFixture.bootstrap.calendars.filter((calendar) => calendar.can_read).map((calendar) => ({ provider: calendar.provider, calendar_id: calendar.id, complete: true, status: "ready" })), complete: true } };
}

/** Intercepts every UI API request so the browser suite never reaches auth or production data. */
export async function installCalendarApiFixture(page: Page, options: CalendarFixtureOptions = {}) {
  const state = options.state ?? "populated";
  await page.unroute("**/api/ui/**");
  await page.route("**/api/ui/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const fixtureBootstrap = state === "no-writable"
      ? { ...calendarFixture.bootstrap, calendars: calendarFixture.bootstrap.calendars.map((calendar) => ({ ...calendar, can_write: false })), capabilities: Object.fromEntries(Object.entries(calendarFixture.bootstrap.capabilities).map(([id, capability]) => [id, { ...capability, read_only: true, operations: { ...capability.operations, create: false, update: false, delete: false } }])) }
      : calendarFixture.bootstrap;
    if (url.pathname.endsWith("/bootstrap")) return route.fulfill({ json: fixtureBootstrap });
    if (url.pathname.endsWith("/events") && request.method() === "GET") {
      const result = responseForState(state);
      return route.fulfill({ status: result.status, json: result.body });
    }
    if (url.pathname.endsWith("/events") && request.method() === "POST") return route.fulfill({ json: { ...calendarFixture.events[0], id: "created-event", title: "Created fixture event" } });
    if (url.pathname.endsWith("/event") && request.method() === "PATCH") return route.fulfill({ json: { event: calendarFixture.events[0] } });
    if (url.pathname.endsWith("/event") && request.method() === "DELETE") return route.fulfill({ json: {} });
    if (/\/calendars\/[^/]+\/refresh$/.test(url.pathname) && request.method() === "POST") return route.fulfill({ status: 204 });
    return route.fulfill({ status: 404, json: { error: { message: `Unhandled visual fixture route: ${url.pathname}` } } });
  });
}
