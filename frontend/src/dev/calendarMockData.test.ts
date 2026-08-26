import { describe, expect, it } from "vitest";
import { createCalendarMockStore } from "./calendarMockData";

const request = (method: string, pathname: string, body?: Record<string, unknown>, query = "") => ({ method, pathname, searchParams: new URLSearchParams(query), body });

describe("local Calendar mock store", () => {
  it("keeps create, update, and delete in its in-memory event list", () => {
    const store = createCalendarMockStore();
    const created = store.handle(request("POST", "/api/ui/events", {
      calendar_id: "google:primary",
      title: "Polish the calendar",
      start: { date_time: "2026-08-25T15:00:00+02:00", time_zone: "Europe/Belgrade" },
      end: { date_time: "2026-08-25T16:00:00+02:00", time_zone: "Europe/Belgrade" },
    }));
    const createdEvent = created.body as { id: string; calendar_id: string; title: string };
    expect(created.status).toBe(200);
    expect(createdEvent.title).toBe("Polish the calendar");

    const updated = store.handle(request("PATCH", "/api/ui/event", { title: "Polished" }, `calendar_id=${createdEvent.calendar_id}&event_id=${createdEvent.id}`));
    expect((updated.body as { event: { title: string } }).event.title).toBe("Polished");

    expect(store.handle(request("DELETE", "/api/ui/event", undefined, `calendar_id=${createdEvent.calendar_id}&event_id=${createdEvent.id}`)).status).toBe(200);
    expect(store.handle(request("GET", "/api/ui/event", undefined, `calendar_id=${createdEvent.calendar_id}&event_id=${createdEvent.id}`)).status).toBe(404);
  });

  it("can expose failure states used by the visual audit without changing the raw fixture", () => {
    const store = createCalendarMockStore("degraded");
    const result = store.handle(request("GET", "/api/ui/events"));
    expect(result.status).toBe(200);
    expect(result.body).toMatchObject({ complete: false, sources: [{ status: "degraded" }] });
  });
});
