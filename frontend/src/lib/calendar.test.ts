import { describe, expect, it } from "vitest";
import { toCalendarEvent, toCreatePayload, toReschedulePayload, toUpdatePayload } from "./calendar";
import type { CalendarRecord, EventDraft, EventRecord } from "./types";

const calendar: CalendarRecord = { id: "google:primary", name: "Personal", provider: "google", color: "#4762ee", capability: { read: true, create: true, write: true, delete: true, recurring: true } };
const event: EventRecord = { id: "evt-1", calendarId: calendar.id, title: "Review", start: "2026-09-15T10:00:00+02:00", end: "2026-09-15T11:00:00+02:00", allDay: false, timezone: "Europe/Belgrade", etag: "v1" };
const draft: EventDraft = { title: "Review", description: "Agenda", location: "Room B", start: "2026-09-15T10:00", end: "2026-09-15T11:00", allDay: false };

describe("calendar mapping", () => {
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
    const payload = toCreatePayload(calendar.id, draft);
    expect(payload).toMatchObject({ calendar_id: calendar.id, title: "Review", start: { date_time: expect.any(String), time_zone: expect.any(String) }, end: { date_time: expect.any(String), time_zone: expect.any(String) } });
    expect(payload).not.toHaveProperty("event");
    expect(payload).not.toHaveProperty("attendees");
    expect(payload).not.toHaveProperty("notifications");
  });

  it("includes the ETag on updates and reschedules", () => {
    expect(toUpdatePayload(event, draft)).toMatchObject({ expected_etag: "v1", scope: "single", title: "Review", start: { date_time: expect.any(String) } });
    expect(toReschedulePayload(event, new Date("2026-09-15T11:00:00+02:00"), new Date("2026-09-15T12:00:00+02:00"))).toMatchObject({ expected_etag: "v1", scope: "single", start: { date_time: expect.any(String) }, end: { date_time: expect.any(String) } });
  });
});
