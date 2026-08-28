import { describe, expect, it } from "vitest";
import { EVENT_STATUS_POLL_INTERVAL_MS, EVENT_STATUS_POLL_MAX_ATTEMPTS, canWriteEvent, currentTimeScrollTime, eventStatusPollInterval, formatEventDate, formatRelativeTime, hasActiveEventSync, selectedReadableCalendarIds, sortCalendarIds, summarizeEventSources, toCalendarEvent, toCreatePayload, toReschedulePayload, toUpdatePayload, toggleAllDayDraft, withMutationScope } from "./calendar";
import type { CalendarRecord, EventDraft, EventRecord } from "./types";

const calendar: CalendarRecord = { id: "google:primary", name: "Personal", provider: "google", color: "#4762ee", capability: { read: true, create: true, write: true, delete: true, recurring: true } };
const event: EventRecord = { id: "evt-1", calendarId: calendar.id, title: "Review", start: "2026-09-15T10:00:00+02:00", end: "2026-09-15T11:00:00+02:00", allDay: false, timezone: "Europe/Belgrade", etag: "v1" };
const draft: EventDraft = { title: "Review", description: "Agenda", location: "Room B", start: "2026-09-15T10:00", end: "2026-09-15T11:00", allDay: false };

function withTimezone<T>(timezone: string, run: () => T): T {
  const runtime = globalThis as typeof globalThis & { process?: { env: Record<string, string | undefined> } };
  if (!runtime.process) throw new Error("This test requires the Node test environment");
  const previous = runtime.process.env.TZ;
  runtime.process.env.TZ = timezone;
  try {
    return run();
  } finally {
    if (previous === undefined) delete runtime.process.env.TZ;
    else runtime.process.env.TZ = previous;
  }
}

describe("calendar mapping", () => {
  it("opens timed calendar views at the current local time", () => {
    expect(currentTimeScrollTime(new Date(2026, 7, 25, 14, 7))).toBe("14:07:00");
  });

  it("maps timed events to FullCalendar without losing editability", () => {
    const mapped = toCalendarEvent(event, calendar);
    expect(mapped).toMatchObject({ id: "google:primary\u0000evt-1", start: event.start, end: event.end, allDay: false, editable: true });
    expect(mapped.extendedProps).toMatchObject({ event, calendar });
  });

  it("keeps all-day end dates exclusive and date-based", () => {
    const allDay: EventRecord = { ...event, start: "2026-09-15", end: "2026-09-17", allDay: true };
    const mapped = toCalendarEvent(allDay, calendar);
    expect(mapped).toMatchObject({ start: "2026-09-15", end: "2026-09-17", allDay: true });
  });

  it("creates a provider-safe payload without deferred fields", () => {
    const payload = withTimezone("Europe/Belgrade", () => toCreatePayload(calendar.id, draft));
    expect(payload).toMatchObject({ calendar_id: calendar.id, title: "Review", start: { date_time: "2026-09-15T08:00:00.000Z", time_zone: "Europe/Belgrade" }, end: { date_time: "2026-09-15T09:00:00.000Z", time_zone: "Europe/Belgrade" } });
    expect(payload).not.toHaveProperty("event");
    expect(payload).not.toHaveProperty("attendees");
    expect(payload).not.toHaveProperty("notifications");
  });

  it("creates all-day payloads with dates and no time-zone fields", () => {
    const payload = toCreatePayload(calendar.id, { ...draft, start: "2026-09-15", end: "2026-09-17", allDay: true });
    expect(payload.start).toEqual({ date: "2026-09-15" });
    expect(payload.end).toEqual({ date: "2026-09-17" });
    expect(payload.start).not.toHaveProperty("date_time");
    expect(payload.start).not.toHaveProperty("time_zone");
  });

  it("includes the ETag on updates and reschedules", () => {
    expect(toUpdatePayload(event, draft)).toMatchObject({ expected_etag: "v1", scope: "single", title: "Review", start: { date_time: expect.any(String) } });
    expect(toReschedulePayload(event, new Date("2026-09-15T11:00:00+02:00"), new Date("2026-09-15T12:00:00+02:00"))).toMatchObject({ expected_etag: "v1", scope: "single", start: { date_time: expect.any(String) }, end: { date_time: expect.any(String) } });
    expect(toReschedulePayload({ ...event, originalStart: { date_time: "2026-09-15T10:00:00+02:00" } }, new Date("2026-09-15T11:00:00+02:00"), new Date("2026-09-15T12:00:00+02:00"))).not.toHaveProperty("effective_from");
  });

  it("adds the recurrence boundary only after following scope is confirmed", () => {
    const recurring = { ...event, originalStart: { date_time: "2026-09-15T10:00:00+02:00", time_zone: "Europe/Belgrade" } };
    const payload = toUpdatePayload(recurring, draft);
    expect(withMutationScope(recurring, payload, "following")).toMatchObject({ scope: "following", effective_from: recurring.originalStart });
    expect(withMutationScope(recurring, payload, "single")).not.toHaveProperty("effective_from");
    expect(withMutationScope(recurring, payload, "series")).not.toHaveProperty("effective_from");
  });

  it("uses the browser-local date when rescheduling an all-day event", () => {
    const allDay: EventRecord = { ...event, allDay: true, start: "2026-09-15", end: "2026-09-16" };
    const payload = withTimezone("Europe/Belgrade", () => toReschedulePayload(allDay, new Date("2026-09-14T22:00:00Z"), new Date("2026-09-15T22:00:00Z")));
    expect(payload.start).toEqual({ date: "2026-09-15" });
    expect(payload.end).toEqual({ date: "2026-09-16" });
  });

  it("keeps all-day display dates stable west of UTC", () => {
    const allDay: EventRecord = { ...event, allDay: true, start: "2026-09-15", end: "2026-09-16" };
    expect(withTimezone("America/Los_Angeles", () => formatEventDate(allDay, "en-US"))).toContain("September 15, 2026");
  });

  it("converts visible form values when all-day mode changes", () => {
    const allDay = toggleAllDayDraft(draft, true);
    expect(allDay).toMatchObject({ allDay: true, start: "2026-09-15", end: "2026-09-16" });
    expect(toggleAllDayDraft(allDay, false)).toMatchObject({ allDay: false, start: "2026-09-15T00:00", end: "2026-09-16T00:00" });
  });

  it("filters persisted selections to readable calendars", () => {
    const readOnly: CalendarRecord = { ...calendar, id: "google:readonly", capability: { ...calendar.capability, read: true, write: false } };
    const unreadable: CalendarRecord = { ...calendar, id: "google:hidden", capability: { ...calendar.capability, read: false } };
    expect(selectedReadableCalendarIds([calendar, readOnly, unreadable], ["missing", readOnly.id, unreadable.id])).toEqual([readOnly.id]);
    expect(selectedReadableCalendarIds([calendar, unreadable], ["missing", unreadable.id])).toEqual([calendar.id]);
    expect(selectedReadableCalendarIds([calendar, unreadable], null)).toEqual([calendar.id]);
  });

  it("sorts calendar IDs and requires a writable calendar for mutations", () => {
    expect(sortCalendarIds(["z", "a", "m"])).toEqual(["a", "m", "z"]);
    expect(canWriteEvent(event, calendar)).toBe(true);
    expect(canWriteEvent({ ...event, readOnly: true }, calendar)).toBe(false);
    expect(canWriteEvent(event, { capability: { ...calendar.capability, write: false } })).toBe(false);
  });

  it("prioritizes failed, stale, syncing, then updated source status", () => {
    const now = new Date("2026-09-15T12:00:00Z");
    const base = { provider: "google", calendar_id: "c1", complete: true, status: "ready" as const, last_success_at: "2026-09-15T11:00:00Z" };
    expect(summarizeEventSources({ complete: true, sources: [{ ...base, status: "syncing" }, { ...base, stale: true }] }, now).kind).toBe("stale");
    expect(summarizeEventSources({ complete: true, sources: [{ ...base, status: "syncing", complete: false }] }, now).label).toBe("Syncing");
    expect(summarizeEventSources({ complete: false, sources: [{ ...base, status: "failed", complete: false, error_code: "transient", next_sync_at: "2026-09-15T12:01:00Z", stale: true }] }, now).label).toBe("Syncing");
    expect(summarizeEventSources({ complete: false, sources: [{ ...base, status: "failed", complete: false, error_code: "rate_limited", next_sync_at: "2026-09-15T12:01:00Z" }] }, now).kind).toBe("syncing");
    expect(summarizeEventSources({ complete: false, sources: [{ ...base, status: "failed", complete: false, error_code: "transient", next_sync_at: "2026-09-15T11:59:59Z" }] }, now).label).toBe("1 calendar failed");
    expect(summarizeEventSources({ complete: false, sources: [{ ...base, status: "failed", complete: false, error_code: "transient", next_sync_at: "not-a-date" }] }, now).label).toBe("1 calendar failed");
    expect(summarizeEventSources({ complete: false, sources: [{ ...base, status: "parked", complete: false, error_code: "auth", next_sync_at: "2026-09-15T12:01:00Z" }] }, now).label).toBe("1 calendar failed");
    expect(summarizeEventSources({ complete: true, sources: [{ ...base, status: "failed" }, { ...base, stale: true }] }, now).label).toBe("1 calendar failed");
    expect(summarizeEventSources({ complete: false, sources: [{ ...base, calendar_id: "c1", status: "degraded", complete: false }] }, now)).toMatchObject({ kind: "degraded", label: "1 calendar needs repair", degradedCalendarIds: ["c1"] });
    expect(summarizeEventSources({ complete: true, sources: [base] }, now).label).toBe("Updated 1 hr ago");
    expect(summarizeEventSources({ complete: true, sources: [{ ...base, last_success_at: "2026-09-15T11:30:00Z" }, { ...base, last_success_at: "2026-09-15T10:45:00+02:00" }] }, now).label).toBe("Updated 30 min ago");
  });

  it("formats relative freshness times", () => {
    const now = new Date("2026-09-15T12:00:00Z");
    expect(formatRelativeTime("2026-09-15T11:59:40Z", now)).toBe("just now");
    expect(formatRelativeTime("2026-09-15T11:42:00Z", now)).toBe("18 min ago");
    expect(formatRelativeTime("2026-09-14T12:00:00Z", now)).toBe("1 day ago");
  });

  it("bounds status polling and allows a manual refresh reset", () => {
    const syncing = [{ provider: "google", calendar_id: "c1", complete: false, status: "syncing" as const }];
    const pendingAndStale = [{ ...syncing[0], status: "pending" as const, stale: true }];
    expect(eventStatusPollInterval(syncing, 0)).toBe(EVENT_STATUS_POLL_INTERVAL_MS);
    expect(eventStatusPollInterval(syncing, EVENT_STATUS_POLL_MAX_ATTEMPTS)).toBe(false);
    expect(eventStatusPollInterval(syncing, 0)).toBe(EVENT_STATUS_POLL_INTERVAL_MS);
    expect(eventStatusPollInterval(pendingAndStale, EVENT_STATUS_POLL_MAX_ATTEMPTS - 1)).toBe(EVENT_STATUS_POLL_INTERVAL_MS);
    expect(eventStatusPollInterval(pendingAndStale, EVENT_STATUS_POLL_MAX_ATTEMPTS)).toBe(false);
    const retrying = [{ ...syncing[0], status: "failed" as const, error_code: "transient", next_sync_at: "2026-09-15T12:01:00Z" }];
    expect(hasActiveEventSync(retrying, new Date("2026-09-15T12:00:00Z"))).toBe(true);
    expect(hasActiveEventSync(retrying, new Date("2026-09-15T12:02:00Z"))).toBe(false);
    expect(eventStatusPollInterval(retrying, 0, EVENT_STATUS_POLL_MAX_ATTEMPTS, new Date("2026-09-15T12:00:00Z"))).toBe(EVENT_STATUS_POLL_INTERVAL_MS);
    expect(eventStatusPollInterval(retrying, EVENT_STATUS_POLL_MAX_ATTEMPTS, EVENT_STATUS_POLL_MAX_ATTEMPTS, new Date("2026-09-15T12:00:00Z"))).toBe(false);
    expect(eventStatusPollInterval(retrying, 0, EVENT_STATUS_POLL_MAX_ATTEMPTS, new Date("2026-09-15T12:02:00Z"))).toBe(false);
    expect(eventStatusPollInterval([{ ...syncing[0], status: "ready" as const }], 0)).toBe(false);
  });
});
