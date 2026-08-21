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

export function sortCalendarIds(calendarIds: string[]): string[] {
  return [...calendarIds].sort();
}

export function selectedReadableCalendarIds(calendars: CalendarRecord[], saved: unknown): string[] {
  const readable = new Set(calendars.filter((calendar) => calendar.capability.read).map((calendar) => calendar.id));
  const allReadable = [...readable];
  if (Array.isArray(saved)) {
    const valid = saved.filter((id): id is string => typeof id === "string" && readable.has(id));
    return valid.length ? valid : allReadable;
  }
  return allReadable;
}

export function canWriteEvent(event: Pick<EventRecord, "readOnly">, calendar?: Pick<CalendarRecord, "capability">): boolean {
  return !event.readOnly && Boolean(calendar?.capability.write);
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

function toLocalDateValue(value: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${value.getFullYear()}-${pad(value.getMonth() + 1)}-${pad(value.getDate())}`;
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
  const startValue = event.allDay ? toLocalDateValue(start) : toLocalInputValue(start.toISOString());
  const endValue = event.allDay ? toLocalDateValue(end) : toLocalInputValue(end.toISOString());
  return {
    start: toEventTime(startValue, event.allDay, event.timezone),
    end: toEventTime(endValue, event.allDay, event.timezone),
    scope: "single",
    ...(event.etag ? { expected_etag: event.etag } : {}),
  };
}

export function withMutationScope(event: EventRecord, payload: EventUpdateRequest, scope: "single" | "following" | "series"): EventUpdateRequest {
  return {
    ...payload,
    scope,
    ...(scope === "following" && event.originalStart ? { effective_from: event.originalStart } : {}),
  };
}

export function toggleAllDayDraft(draft: EventDraft, allDay: boolean): EventDraft {
  if (draft.allDay === allDay) return draft;
  if (!allDay) {
    return { ...draft, allDay, start: draft.start ? `${draft.start.slice(0, 10)}T00:00` : "", end: draft.end ? `${draft.end.slice(0, 10)}T00:00` : "" };
  }
  const start = draft.start.slice(0, 10);
  let end = draft.end.slice(0, 10);
  if (start && (!end || end <= start)) {
    const next = new Date(`${start}T00:00:00Z`);
    next.setUTCDate(next.getUTCDate() + 1);
    end = next.toISOString().slice(0, 10);
  }
  return { ...draft, allDay, start, end };
}

export function formatEventTime(event: EventRecord, locale?: Intl.LocalesArgument): string {
  if (event.allDay) return "All day";
  const formatter = new Intl.DateTimeFormat(locale, { hour: "numeric", minute: "2-digit" });
  return `${formatter.format(new Date(event.start))} – ${formatter.format(new Date(event.end))}`;
}

export function formatEventDate(event: EventRecord, locale?: Intl.LocalesArgument): string {
  let value: Date;
  if (event.allDay && /^\d{4}-\d{2}-\d{2}$/.test(event.start)) {
    const [year, month, day] = event.start.split("-").map(Number);
    value = new Date(year, month - 1, day);
  } else {
    value = new Date(event.start);
  }
  return new Intl.DateTimeFormat(locale, { weekday: "long", month: "long", day: "numeric", year: "numeric" }).format(value);
}
