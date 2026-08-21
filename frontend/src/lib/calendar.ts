import type { EventInput } from "@fullcalendar/core";
import type { CalendarRecord, EventCreateRequest, EventDraft, EventRecord, EventTime, EventUpdateRequest } from "./types";

export function toCalendarEvent(event: EventRecord, calendar?: CalendarRecord): EventInput {
  return {
    id: calendarEventKey(event),
    title: event.title || "Untitled event",
    start: event.start,
    end: event.end,
    allDay: event.allDay,
    editable: !event.readOnly && Boolean(calendar?.capability.write),
    classNames: [event.allDay ? "calendar-event--all-day" : "calendar-event--timed"],
    backgroundColor: calendar?.color ?? "#4762ee",
    borderColor: calendar?.color ?? "#4762ee",
    extendedProps: { event, calendar },
  };
}

export function calendarEventKey(event: Pick<EventRecord, "calendarId" | "id">): string {
  return `${event.calendarId}\u0000${event.id}`;
}

export function toLocalInputValue(value: string, allDay = false): string {
  if (allDay) return value.slice(0, 10);
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value.slice(0, 16);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function toEventDraft(event?: EventRecord): EventDraft {
  if (!event) {
    const start = new Date();
    start.setMinutes(Math.ceil(start.getMinutes() / 30) * 30, 0, 0);
    const end = new Date(start.getTime() + 60 * 60 * 1000);
    return {
      title: "",
      description: "",
      location: "",
      start: toLocalInputValue(start.toISOString()),
      end: toLocalInputValue(end.toISOString()),
      allDay: false,
    };
  }
  return {
    title: event.title,
    description: event.description ?? "",
    location: event.location ?? "",
    start: toLocalInputValue(event.start, event.allDay),
    end: toLocalInputValue(event.end, event.allDay),
    allDay: event.allDay,
  };
}

function toEventTime(value: string, allDay: boolean, timezone?: string): EventTime {
  if (allDay) return { date: value.slice(0, 10) };
  return { date_time: new Date(value).toISOString(), time_zone: timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone };
}

export function toCreatePayload(calendarId: string, draft: EventDraft): EventCreateRequest {
  const event: EventCreateRequest = {
    calendar_id: calendarId,
    title: draft.title.trim(),
    start: toEventTime(draft.start, draft.allDay),
    end: toEventTime(draft.end, draft.allDay),
  };
  if (draft.description.trim()) event.description = draft.description.trim();
  if (draft.location.trim()) event.location = draft.location.trim();
  return event;
}

export function toUpdatePayload(event: EventRecord, draft: EventDraft): EventUpdateRequest {
  return {
    title: draft.title.trim(),
    description: draft.description.trim(),
    location: draft.location.trim(),
    start: toEventTime(draft.start, draft.allDay, event.timezone),
    end: toEventTime(draft.end, draft.allDay, event.timezone),
    scope: "single",
    ...(event.etag ? { expected_etag: event.etag } : {}),
  };
}

export function toReschedulePayload(event: EventRecord, start: Date, end: Date): EventUpdateRequest {
  return {
    start: toEventTime(toLocalInputValue(start.toISOString(), event.allDay), event.allDay, event.timezone),
    end: toEventTime(toLocalInputValue(end.toISOString(), event.allDay), event.allDay, event.timezone),
    scope: "single",
    ...(event.etag ? { expected_etag: event.etag } : {}),
    ...(event.originalStart ? { effective_from: event.originalStart } : {}),
  };
}

export function formatEventTime(event: EventRecord, locale = undefined): string {
  if (event.allDay) return "All day";
  const formatter = new Intl.DateTimeFormat(locale, { hour: "numeric", minute: "2-digit" });
  return `${formatter.format(new Date(event.start))} – ${formatter.format(new Date(event.end))}`;
}

export function formatEventDate(event: EventRecord, locale = undefined): string {
  return new Intl.DateTimeFormat(locale, { weekday: "long", month: "long", day: "numeric", year: "numeric" }).format(new Date(event.start));
}
