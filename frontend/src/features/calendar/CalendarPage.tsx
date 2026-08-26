import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import FullCalendar from "@fullcalendar/react";
import dayGridPlugin from "@fullcalendar/daygrid";
import timeGridPlugin from "@fullcalendar/timegrid";
import listPlugin from "@fullcalendar/list";
import interactionPlugin from "@fullcalendar/interaction";
import type { DatesSetArg, EventClickArg, EventContentArg, EventDropArg, DateSelectArg } from "@fullcalendar/core";
import type { EventResizeDoneArg } from "@fullcalendar/interaction";
import { AlignLeft, CalendarDays, ChevronLeft, ChevronRight, Clock3, ListFilter, MapPin, MoreHorizontal, Plus, RefreshCw, Video, X } from "lucide-react";
import { NavLink } from "react-router-dom";
import { useBootstrapData } from "../../app/App";
import { APIError, createEvent, deleteEvent, getEvents, isSessionExpiredError, navigateToApp, refreshCalendars, updateEvent } from "../../lib/api";
import { EVENT_STATUS_POLL_MAX_ATTEMPTS, calendarEventKey, canWriteEvent, currentTimeScrollTime, eventStatusPollInterval, formatEventDate, formatEventTime, selectedReadableCalendarIds, sortCalendarIds, summarizeEventSources, toCalendarEvent, toCreatePayload, toEventDraft, toLocalInputValue, toReschedulePayload, toUpdatePayload, toggleAllDayDraft, withMutationScope } from "../../lib/calendar";
import type { CalendarRecord, EventDraft, EventRecord } from "../../lib/types";
import { ErrorState, EmptyState, LoadingState } from "../../components/AsyncState";
import { HtmlDescription } from "../../components/HtmlDescription";

type ViewName = "timeGridDay" | "timeGridWeek" | "dayGridMonth" | "listWeek";
type Range = { start: string; end: string } | null;
type ScopeAction = "reschedule" | "edit" | "delete";
type Surface =
  | { kind: "none" }
  | { kind: "filters" }
  | { kind: "event"; event: EventRecord }
  | { kind: "create"; preset?: Partial<EventDraft> }
  | { kind: "scope"; event: EventRecord; action: ScopeAction };
type ScopePayload = { revert?: () => void; payload?: ReturnType<typeof toReschedulePayload> | ReturnType<typeof toUpdatePayload> };
type Notice = { message: string; intent: "success" | "warning" | "error" } | null;
type MonthDay = { date: Date; key: string; events: EventRecord[] };

const CALENDAR_VIEW_STORAGE_KEY = "calendar:view";
const SUCCESS_STATUS_DURATION_MS = 4_000;
export const COMPACT_LAYOUT_MAX_WIDTH = 1439;
const COMPACT_LAYOUT_QUERY = `(max-width: ${COMPACT_LAYOUT_MAX_WIDTH}px)`;
const NARROW_CALENDAR_QUERY = "(max-width: 760px)";
const PHONE_CALENDAR_QUERY = "(max-width: 560px)";
const usingCalendarMocks = import.meta.env.DEV && import.meta.env.VITE_CALENDAR_MOCKS === "true";
const MOBILE_MONTH_EVENT_LIMIT = 2;

export function usesCompactCalendarLayout(viewportWidth: number) {
  return viewportWidth <= COMPACT_LAYOUT_MAX_WIDTH;
}

export function calendarDayKey(date: Date, timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone) {
  const parts = new Intl.DateTimeFormat("en-CA", { timeZone, year: "numeric", month: "2-digit", day: "2-digit" }).formatToParts(date);
  const value = (type: Intl.DateTimeFormatPartTypes) => parts.find((part) => part.type === type)?.value;
  return `${value("year")}-${value("month")}-${value("day")}`;
}

export function eventDayKey(event: EventRecord) {
  return event.allDay ? event.start.slice(0, 10) : calendarDayKey(new Date(event.start), event.timezone);
}

function eventDayKeys(event: EventRecord) {
  if (!event.allDay) return [eventDayKey(event)];
  const start = event.start.slice(0, 10);
  const end = event.end.slice(0, 10);
  if (!/^\d{4}-\d{2}-\d{2}$/.test(start) || !/^\d{4}-\d{2}-\d{2}$/.test(end) || end <= start) return [start];
  const days: string[] = [];
  const cursor = new Date(`${start}T00:00:00`);
  const lastExclusive = new Date(`${end}T00:00:00`);
  while (cursor < lastExclusive) {
    days.push(`${cursor.getFullYear()}-${String(cursor.getMonth() + 1).padStart(2, "0")}-${String(cursor.getDate()).padStart(2, "0")}`);
    cursor.setDate(cursor.getDate() + 1);
  }
  return days;
}

export function monthDays(visibleDate: Date, events: EventRecord[]): MonthDay[] {
  const year = visibleDate.getFullYear();
  const month = visibleDate.getMonth();
  const firstWeekday = (new Date(year, month, 1).getDay() + 6) % 7;
  const byDay = new Map<string, EventRecord[]>();
  events.forEach((event) => eventDayKeys(event).forEach((key) => byDay.set(key, [...(byDay.get(key) ?? []), event])));
  return Array.from({ length: 42 }, (_, index) => {
    const day = index - firstWeekday + 1;
    const date = new Date(year, month, day);
    const key = calendarDayKey(date);
    return { date, key, events: day > 0 && day <= new Date(year, month + 1, 0).getDate() ? byDay.get(key) ?? [] : [] };
  });
}

const VIEW_OPTIONS: Array<{ value: ViewName; label: string }> = [
  { value: "timeGridDay", label: "Day" },
  { value: "timeGridWeek", label: "Week" },
  { value: "dayGridMonth", label: "Month" },
  { value: "listWeek", label: "List" },
];

export default function CalendarPage() {
  const { csrf_token: csrfToken, calendars } = useBootstrapData();
  const calendarRef = useRef<FullCalendar>(null);
  const initialScrollTime = useMemo(() => currentTimeScrollTime(), []);
  const queryClient = useQueryClient();
  const [range, setRange] = useState<Range>(null);
  const [miniCalendarDate, setMiniCalendarDate] = useState(() => new Date());
  const [view, setView] = useState<ViewName>(() => {
    try {
      const saved = localStorage.getItem(CALENDAR_VIEW_STORAGE_KEY);
      if (VIEW_OPTIONS.some((option) => option.value === saved)) return saved as ViewName;
    } catch { /* use responsive default */ }
    return window.matchMedia?.("(max-width: 760px)").matches ? "timeGridDay" : "timeGridWeek";
  });
  const [selectedCalendarIds, setSelectedCalendarIds] = useState<string[]>(() => {
    try {
      const saved = JSON.parse(localStorage.getItem("calendar:selected") ?? "null");
      const selected = selectedReadableCalendarIds(calendars, saved);
      if (Array.isArray(saved)) localStorage.setItem("calendar:selected", JSON.stringify(selected));
      return selected;
    } catch { /* use all calendars */ }
    return selectedReadableCalendarIds(calendars, null);
  });
  // Surface state is intentionally serializable: future deep-link support can
  // map date/view/filter/event without introducing a second ownership model.
  const compactLayout = useMediaQuery(COMPACT_LAYOUT_QUERY);
  const narrowCalendarLayout = useMediaQuery(NARROW_CALENDAR_QUERY);
  const phoneCalendarLayout = useMediaQuery(PHONE_CALENDAR_QUERY);
  const [surface, setSurface] = useState<Surface>({ kind: "none" });
  const scopePayloadRef = useRef<ScopePayload | null>(null);
  const [editEvent, setEditEvent] = useState<EventRecord | null>(null);
  const [notice, setNotice] = useState<Notice>(null);
  const [showUpdatedStatus, setShowUpdatedStatus] = useState(false);
  const [deleteConfirm, setDeleteConfirm] = useState<EventRecord | null>(null);
  const lastPolledDataUpdatedAtRef = useRef(0);
  const [pollingAttempts, setPollingAttempts] = useState(0);
  const refreshStartedAtRef = useRef(0);
  const [pollingCapped, setPollingCapped] = useState(false);
  const sortedSelectedCalendarIds = useMemo(() => sortCalendarIds(selectedCalendarIds), [selectedCalendarIds]);

  const eventsQuery = useQuery({
    queryKey: ["events", range?.start, range?.end, sortedSelectedCalendarIds],
    queryFn: () => getEvents(range!.start, range!.end, sortedSelectedCalendarIds),
    enabled: Boolean(range && sortedSelectedCalendarIds.length),
    retry: (failureCount, error) => !isSessionExpiredError(error) && failureCount < 1,
    placeholderData: (previous) => previous,
    refetchInterval: (query) => {
      return eventStatusPollInterval(query.state.data?.sources, pollingAttempts);
    },
    refetchIntervalInBackground: false,
  });
  const events = eventsQuery.data?.items ?? [];
  const baseEventStatus = eventsQuery.data ? summarizeEventSources(eventsQuery.data) : null;
  const eventStatus = baseEventStatus?.kind === "syncing" && pollingCapped ? { ...baseEventStatus, kind: "stale" as const, label: "Sync paused; refresh to try again" } : baseEventStatus;
  const visibleEventStatus = eventStatus?.kind === "updated" && !showUpdatedStatus ? null : eventStatus;
  const hasPendingSources = eventsQuery.data?.sources?.some((source) => source.status === "pending" || source.status === "syncing") ?? false;
  const eventsById = useMemo(() => new Map(events.map((event) => [calendarEventKey(event), event])), [events]);
  const calendarById = useMemo(() => new Map(calendars.map((calendar) => [calendar.id, calendar])), [calendars]);
  const calendarEvents = useMemo(() => events.map((event) => toCalendarEvent(event, calendarById.get(event.calendarId))), [events, calendarById]);
  const showMobileMonth = phoneCalendarLayout && view === "dayGridMonth";
  const mobileMonthDays = useMemo(() => monthDays(miniCalendarDate, events), [miniCalendarDate, events]);

  const invalidateEvents = () => void queryClient.invalidateQueries({ queryKey: ["events"] });
  useEffect(() => {
    setPollingAttempts(0);
    setPollingCapped(false);
    lastPolledDataUpdatedAtRef.current = 0;
  }, [range?.start, range?.end, sortedSelectedCalendarIds]);
  const refreshMutation = useMutation({
    mutationFn: (calendarIds: string[]) => refreshCalendars(csrfToken, calendarIds),
    onSuccess: (result) => {
      if (result.sessionExpired.length) {
        const queued = result.queued.length ? `${result.queued.length} refresh${result.queued.length === 1 ? "" : "es"} queued. ` : "";
        setNotice({ message: `${queued}Your session expired; returning to sign in.`, intent: "error" });
        navigateToApp();
        return;
      }
      if (result.queued.length && (result.rejected.length || result.unknown.length)) {
        const rejected = result.rejected.length ? ` ${result.rejected.length} rejected.` : "";
        const unknown = result.unknown.length ? ` ${result.unknown.length} outcome${result.unknown.length === 1 ? "" : "s"} unconfirmed.` : "";
        setNotice({ message: `${result.queued.length} refresh${result.queued.length === 1 ? "" : "es"} queued.${rejected}${unknown} No automatic retry was attempted.`, intent: "warning" });
      } else if (result.queued.length) {
        refreshStartedAtRef.current = eventsQuery.dataUpdatedAt;
        setShowUpdatedStatus(true);
        setNotice({ message: `${result.queued.length} calendar refresh${result.queued.length === 1 ? "" : "es"} queued.`, intent: "success" });
      } else if (result.rejected.length || result.unknown.length) {
        const rejected = result.rejected.length ? `${result.rejected.length} refresh${result.rejected.length === 1 ? "" : "es"} rejected.` : "";
        const unknown = result.unknown.length ? `${result.unknown.length} outcome${result.unknown.length === 1 ? "" : "s"} unconfirmed.` : "";
        setNotice({ message: `${rejected} ${unknown} No automatic retry was attempted.`.trim(), intent: "error" });
      }
    },
    onError: (error) => { if (isSessionExpiredError(error)) navigateToApp(); else setNotice({ message: safeError(error), intent: "error" }); },
    onSettled: invalidateEvents,
  });
  useEffect(() => {
    if (!hasPendingSources) {
      setPollingAttempts(0);
      setPollingCapped(false);
      lastPolledDataUpdatedAtRef.current = eventsQuery.dataUpdatedAt;
      return;
    }
    if (eventsQuery.dataUpdatedAt && eventsQuery.dataUpdatedAt !== lastPolledDataUpdatedAtRef.current) {
      lastPolledDataUpdatedAtRef.current = eventsQuery.dataUpdatedAt;
      setPollingAttempts((current) => {
        const next = Math.min(EVENT_STATUS_POLL_MAX_ATTEMPTS, current + 1);
        if (next >= EVENT_STATUS_POLL_MAX_ATTEMPTS) setPollingCapped(true);
        return next;
      });
    }
  }, [hasPendingSources, eventsQuery.dataUpdatedAt]);
  useEffect(() => {
    if (!showUpdatedStatus || eventStatus?.kind !== "updated" || eventsQuery.dataUpdatedAt <= refreshStartedAtRef.current) return;
    const timeout = window.setTimeout(() => setShowUpdatedStatus(false), SUCCESS_STATUS_DURATION_MS);
    return () => window.clearTimeout(timeout);
  }, [eventStatus?.kind, eventsQuery.dataUpdatedAt, showUpdatedStatus]);
  const createMutation = useMutation({
    mutationFn: (payload: Parameters<typeof createEvent>[1]) => createEvent(csrfToken, payload),
    onSuccess: (event) => { setSurface({ kind: "none" }); setNotice(mutationNotice("Event created", event.warnings)); invalidateEvents(); },
    onError: (error) => { if (isSessionExpiredError(error)) navigateToApp(); else setNotice({ message: safeError(error), intent: "error" }); },
  });
  const updateMutation = useMutation({
    mutationFn: (args: { event: EventRecord; payload: Parameters<typeof updateEvent>[3]; revert?: () => void }) => updateEvent(csrfToken, args.event.calendarId, args.event.id, args.payload),
    onSuccess: (event) => { setSurface((current) => current.kind === "event" ? { kind: "event", event } : current); setEditEvent(null); setNotice(mutationNotice("Event updated", event.warnings)); invalidateEvents(); },
    onError: (error, variables) => { variables.revert?.(); if (isSessionExpiredError(error)) navigateToApp(); else if (error instanceof APIError && error.status === 409) { setNotice({ message: "This event changed elsewhere. We refreshed it; please try again.", intent: "error" }); void queryClient.invalidateQueries({ queryKey: ["events"] }); } else setNotice({ message: safeError(error), intent: "error" }); },
  });
  const deleteMutation = useMutation({
    mutationFn: (args: { event: EventRecord; scope?: "single" | "following" | "series" }) => deleteEvent(csrfToken, args.event.calendarId, args.event.id, { scope: args.scope ?? "single", expected_etag: args.event.etag, effective_from: args.scope === "following" ? args.event.originalStart : undefined }),
    onSuccess: (result) => { scopePayloadRef.current = null; setSurface({ kind: "none" }); setNotice(mutationNotice("Event deleted", result.warnings)); invalidateEvents(); },
    onError: (error) => { if (isSessionExpiredError(error)) navigateToApp(); else setNotice({ message: safeError(error), intent: "error" }); },
  });
  useEffect(() => {
    if (isSessionExpiredError(eventsQuery.error)) navigateToApp();
  }, [eventsQuery.error]);

  function changeView(nextView: ViewName) {
    setView(nextView);
    try { localStorage.setItem(CALENDAR_VIEW_STORAGE_KEY, nextView); } catch { /* persistence is optional */ }
    calendarRef.current?.getApi().changeView(phoneCalendarLayout && nextView === "timeGridWeek" ? "mobileThreeDay" : nextView);
  }

  useEffect(() => {
    if (view !== "timeGridWeek") return;
    calendarRef.current?.getApi().changeView(phoneCalendarLayout ? "mobileThreeDay" : "timeGridWeek");
  }, [phoneCalendarLayout, view]);

  function navigate(direction: "prev" | "next" | "today") {
    const api = calendarRef.current?.getApi();
    if (!api) return;
    if (direction === "today") api.today(); else direction === "prev" ? api.prev() : api.next();
  }
  function toggleCalendar(calendar: CalendarRecord) {
    if (!calendar.capability.read) return;
    setSelectedCalendarIds((current) => {
      const next = current.includes(calendar.id) ? current.filter((id) => id !== calendar.id) : [...current, calendar.id];
      localStorage.setItem("calendar:selected", JSON.stringify(next));
      return next;
    });
  }
  function openCreate(preset?: Partial<EventDraft>) { setSurface({ kind: "create", preset }); }
  function openEdit(event: EventRecord) {
    if (!canWriteEvent(event, calendarById.get(event.calendarId))) return;
    setSurface({ kind: "none" });
    setEditEvent(event);
  }
  function handleSelect(selection: DateSelectArg) {
    const start = selection.allDay ? selection.startStr : toLocalInputValue(selection.start.toISOString());
    const end = selection.allDay ? selection.endStr : toLocalInputValue(selection.end.toISOString());
    openCreate({ start, end, allDay: selection.allDay });
  }
  function handleEventClick(info: EventClickArg) {
    const event = eventsById.get(info.event.id) ?? info.event.extendedProps.event as EventRecord | undefined;
    if (event) setSurface({ kind: "event", event });
  }
  function handleMove(info: EventDropArg | EventResizeDoneArg) {
    const event = eventsById.get(info.event.id) ?? info.event.extendedProps.event as EventRecord | undefined;
    if (!event || !canWriteEvent(event, calendarById.get(event.calendarId)) || !info.event.start || !info.event.end) { info.revert(); return; }
    const payload = toReschedulePayload(event, info.event.start, info.event.end);
    if (event.recurrence?.isRecurring) {
      scopePayloadRef.current = { revert: info.revert, payload };
      setSurface({ kind: "scope", event, action: "reschedule" });
    } else {
      updateMutation.mutate({ event, payload, revert: info.revert });
    }
  }
  function confirmScope(scope: "single" | "following" | "series") {
    if (surface.kind !== "scope") return;
    const { event, action } = surface;
    const { revert, payload } = scopePayloadRef.current ?? {};
    scopePayloadRef.current = null;
    setSurface({ kind: "none" });
    if (action === "delete") deleteMutation.mutate({ event, scope });
    else if ((action === "reschedule" || action === "edit") && payload) updateMutation.mutate({ event, payload: withMutationScope(event, payload, scope), revert });
  }
  function askDelete(event: EventRecord) {
    if (event.recurrence?.isRecurring) setSurface({ kind: "scope", event, action: "delete" });
    else { setSurface({ kind: "none" }); setDeleteConfirm(event); }
  }
  function saveDraft(draft: EventDraft, calendarId: string) {
    if (surface.kind === "create") createMutation.mutate(toCreatePayload(calendarId, draft));
    else if (editEvent) {
      const payload = toUpdatePayload(editEvent, draft);
      if (editEvent.recurrence?.isRecurring) { scopePayloadRef.current = { payload }; setSurface({ kind: "scope", event: editEvent, action: "edit" }); setEditEvent(null); }
      else updateMutation.mutate({ event: editEvent, payload });
    }
  }
  function refreshCalendarIds(candidateIds: string[]) {
    const readable = calendars.filter((calendar) => calendar.capability.read && candidateIds.includes(calendar.id)).map((calendar) => calendar.id);
    if (readable.length) {
      setPollingAttempts(0);
      setPollingCapped(false);
      lastPolledDataUpdatedAtRef.current = 0;
      refreshMutation.mutate(readable);
    }
  }

  function refreshSelectedCalendars() {
    refreshCalendarIds(sortedSelectedCalendarIds);
  }

  function refreshDegradedCalendars() {
    const degraded = eventStatus?.kind === "degraded" ? eventStatus.degradedCalendarIds ?? [] : [];
    refreshCalendarIds(degraded);
  }

  function showAllCalendars() {
    const next = calendars.filter((calendar) => calendar.capability.read).map((calendar) => calendar.id);
    setSelectedCalendarIds(next);
    try { localStorage.setItem("calendar:selected", JSON.stringify(next)); } catch { /* persistence is optional */ }
  }

  const sheetOpen = compactLayout && (surface.kind === "filters" || surface.kind === "event");
  const persistentSidebar = !compactLayout;
  return <div className={`calendar-screen ${persistentSidebar || surface.kind === "filters" ? "with-sidebar" : "wide"} ${surface.kind === "event" ? "with-drawer" : ""}`}>
    {persistentSidebar && <aside className="calendar-sidebar is-open" aria-label="Calendar filters"><CalendarSidebar showClose={false} visibleDate={miniCalendarDate} calendars={calendars} selected={selectedCalendarIds} onToggle={toggleCalendar} onCreate={() => openCreate()} onRefresh={refreshSelectedCalendars} refreshDisabled={refreshMutation.isPending || !sortedSelectedCalendarIds.length} refreshing={refreshMutation.isPending} onClose={() => undefined} onJumpToDate={(date) => { calendarRef.current?.getApi().gotoDate(date); }} /></aside>}
    {surface.kind === "filters" && compactLayout && <SurfaceSheet className="calendar-sidebar" labelledBy="calendar-filters-title" onClose={() => setSurface({ kind: "none" })}><CalendarSidebar visibleDate={miniCalendarDate} calendars={calendars} selected={selectedCalendarIds} onToggle={toggleCalendar} onCreate={() => openCreate()} onRefresh={refreshSelectedCalendars} refreshDisabled={refreshMutation.isPending || !sortedSelectedCalendarIds.length} refreshing={refreshMutation.isPending} onClose={() => setSurface({ kind: "none" })} onJumpToDate={(date) => { calendarRef.current?.getApi().gotoDate(date); setSurface({ kind: "none" }); }} /></SurfaceSheet>}
    <main className="calendar-main" inert={sheetOpen} aria-hidden={sheetOpen || undefined}>
      <div className="calendar-toolbar">
        <div className="toolbar-primary"><button className="button button-outline today-button" onClick={() => navigate("today")}>Today</button><div className="nav-arrows"><button className="icon-button bordered" onClick={() => navigate("prev")} aria-label="Previous period"><ChevronLeft size={19} /></button><button className="icon-button bordered" onClick={() => navigate("next")} aria-label="Next period"><ChevronRight size={19} /></button></div><span className="date-title" aria-live="polite">{calendarRef.current?.getApi().view.title ?? "Calendar"}</span></div>
        <div className="toolbar-actions"><div className="toolbar-scroll-controls">{!persistentSidebar && <><button className="button button-primary calendar-create-button" onClick={() => openCreate()} aria-label="New event" title="New event"><Plus size={16} /><span>New event</span></button><button className="button button-outline calendar-filter-button" onClick={() => setSurface({ kind: "filters" })} aria-label="Choose calendars" title="Choose calendars"><CalendarDays size={17} /><span className="calendar-filter-label">Calendars</span></button></>}</div>{usingCalendarMocks && <span className="mock-data-indicator" role="status">Mock data</span>}{phoneCalendarLayout || !narrowCalendarLayout ? <div className="view-switcher" role="group" aria-label="Calendar view">{VIEW_OPTIONS.map((option) => <button key={option.value} className={view === option.value ? "is-selected" : ""} aria-pressed={view === option.value} onClick={() => changeView(option.value)}>{option.label}</button>)}</div> : <label className="compact-view-picker"><span className="visually-hidden">Calendar view</span><select value={view} onChange={(event) => changeView(event.target.value as ViewName)}>{VIEW_OPTIONS.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>}</div>
      </div>
      <div className="calendar-mobile-filter"><button className="button button-outline" onClick={() => setSurface({ kind: "filters" })}><ListFilter size={16} /> Calendars</button><button className="button button-primary" onClick={() => openCreate()}><Plus size={16} /> New event</button></div>
      {visibleEventStatus && <div className={`event-status-strip event-status-row event-status-row--${visibleEventStatus.kind}`} role={visibleEventStatus.kind === "failed" ? "alert" : "status"} aria-live="polite"><span>{visibleEventStatus.label}</span>{(visibleEventStatus.kind === "failed" || visibleEventStatus.kind === "degraded") && <span className="event-status-detail">Cached events remain visible.</span>}{visibleEventStatus.kind === "degraded" && <button className="status-action" type="button" onClick={refreshDegradedCalendars} disabled={refreshMutation.isPending}>Refresh affected calendars</button>}</div>}
      {notice && <div className={`notice notice--${notice.intent}`} role={notice.intent === "error" ? "alert" : "status"}><span>{notice.message}</span><button className="icon-button" onClick={() => setNotice(null)} aria-label="Dismiss notice"><X size={16} /></button></div>}
      {!selectedCalendarIds.length ? <EmptyState title="No calendars selected" message="Choose at least one calendar from the sidebar to see your events." action={<button className="button button-secondary" onClick={showAllCalendars}>Show all calendars</button>} /> : <div className={`calendar-canvas-wrap ${narrowCalendarLayout && !phoneCalendarLayout && view === "timeGridWeek" ? "is-narrow-week" : ""} ${showMobileMonth ? "has-mobile-month" : ""}`}>
        {eventsQuery.isPending && !eventsQuery.data && <div className="calendar-overlay"><LoadingState label="Loading events" /></div>}
        {eventsQuery.isError && !events.length && <div className="calendar-overlay"><ErrorState message={safeError(eventsQuery.error)} retry={() => void eventsQuery.refetch()} /></div>}
        {showMobileMonth && <MobileMonth visibleDate={miniCalendarDate} days={mobileMonthDays} calendars={calendarById} onNavigate={navigate} onSelectDay={(date) => { calendarRef.current?.getApi().gotoDate(date); changeView("timeGridDay"); }} onSelectEvent={(event) => setSurface({ kind: "event", event })} onCreate={() => openCreate()} onFilters={() => setSurface({ kind: "filters" })} />}
        <FullCalendar ref={calendarRef} plugins={[dayGridPlugin, timeGridPlugin, listPlugin, interactionPlugin]} initialView={phoneCalendarLayout && view === "timeGridWeek" ? "mobileThreeDay" : view} initialDate={usingCalendarMocks ? "2026-08-25" : undefined} firstDay={1} scrollTime={initialScrollTime} headerToolbar={false} height="100%" expandRows selectable selectMirror editable eventStartEditable eventDurationEditable nowIndicator slotEventOverlap={false} dayMaxEventRows={3} moreLinkClick="popover" views={{ mobileThreeDay: { type: "timeGrid", duration: { days: 3 } } }} events={calendarEvents} select={handleSelect} eventClick={handleEventClick} eventDrop={handleMove} eventResize={handleMove} datesSet={(arg: DatesSetArg) => { setRange({ start: arg.start.toISOString(), end: arg.end.toISOString() }); setMiniCalendarDate(arg.view.calendar.getDate()); }} eventContent={(arg) => <CalendarEventContent arg={arg} />} />
      </div>}
    </main>
    {surface.kind === "event" && (compactLayout ? <SurfaceSheet className="event-drawer" labelledBy="event-details-title" onClose={() => setSurface({ kind: "none" })}><EventDrawer event={surface.event} calendar={calendarById.get(surface.event.calendarId)} onClose={() => setSurface({ kind: "none" })} onEdit={() => openEdit(surface.event)} onDelete={() => askDelete(surface.event)} /></SurfaceSheet> : <aside className="event-drawer is-open" aria-label="Event details"><EventDrawer event={surface.event} calendar={calendarById.get(surface.event.calendarId)} onClose={() => setSurface({ kind: "none" })} onEdit={() => openEdit(surface.event)} onDelete={() => askDelete(surface.event)} /></aside>)}
    {surface.kind === "create" && <EventModal mode="create" preset={surface.preset} calendars={calendars} onClose={() => setSurface({ kind: "none" })} onSave={saveDraft} busy={createMutation.isPending || updateMutation.isPending} />}
    {editEvent && <EventModal mode="edit" event={editEvent} calendars={calendars} onClose={() => setEditEvent(null)} onSave={saveDraft} busy={createMutation.isPending || updateMutation.isPending} />}
    {surface.kind === "scope" && <ScopeDialog event={surface.event} action={surface.action} scopes={calendarById.get(surface.event.calendarId)?.capability.recurrenceScopes} onCancel={() => { scopePayloadRef.current?.revert?.(); scopePayloadRef.current = null; setSurface({ kind: "none" }); }} onConfirm={confirmScope} />}
    {deleteConfirm && <ConfirmDialog event={deleteConfirm} busy={deleteMutation.isPending} onCancel={() => setDeleteConfirm(null)} onConfirm={() => { const event = deleteConfirm; setDeleteConfirm(null); deleteMutation.mutate({ event }); }} />}
  </div>;
}

function useMediaQuery(query: string) {
  const [matches, setMatches] = useState(() => window.matchMedia?.(query).matches ?? false);
  useEffect(() => {
    const media = window.matchMedia?.(query);
    if (!media) return;
    const update = () => setMatches(media.matches);
    update();
    media.addEventListener?.("change", update);
    return () => media.removeEventListener?.("change", update);
  }, [query]);
  return matches;
}

function safeError(error: unknown) {
  if (!(error instanceof APIError)) return "The request could not be completed.";
  switch (error.code) {
    case "permission_denied": return "Calendar access was denied.";
    case "rate_limited": return "Calendar provider is temporarily rate limited.";
    case "not_found": return "The calendar or event was not found.";
    case "conflict": return "The event changed elsewhere. Please refresh and try again.";
    case "unsupported_capability": return "This calendar operation is not supported.";
    case "invalid_argument":
    case "invalid_recurrence": return "The calendar request is invalid.";
    case "provider_unavailable":
    case "partial_failure": return "Calendar provider is temporarily unavailable.";
    default: return error.status >= 500 ? "Calendar service is temporarily unavailable." : "The request could not be completed.";
  }
}

function mutationNotice(success: string, warnings?: string[]): Notice {
  return { message: warnings?.length ? `${success}. ${warnings.join(" ")}` : success, intent: warnings?.length ? "warning" : "success" };
}

function SurfaceSheet({ children, className, labelledBy, onClose }: { children: ReactNode; className: string; labelledBy: string; onClose: () => void }) {
  const sheetRef = useRef<HTMLElement>(null);
  const previousFocusRef = useRef<HTMLElement | null>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  useEffect(() => {
    previousFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const sheet = sheetRef.current;
    if (!sheet) return;
    const focusable = () => Array.from(sheet.querySelectorAll<HTMLElement>("button, [href], input, select, textarea, [tabindex]:not([tabindex='-1'])")).filter((element) => !element.hasAttribute("disabled"));
    const first = focusable()[0];
    (first ?? sheet).focus();
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") { event.preventDefault(); onCloseRef.current(); return; }
      if (event.key !== "Tab") return;
      const items = focusable();
      if (!items.length) { event.preventDefault(); sheet.focus(); return; }
      const current = document.activeElement;
      const firstItem = items[0];
      const lastItem = items[items.length - 1];
      if (event.shiftKey && current === firstItem) { event.preventDefault(); lastItem.focus(); }
      else if (!event.shiftKey && current === lastItem) { event.preventDefault(); firstItem.focus(); }
    };
    sheet.addEventListener("keydown", handleKeyDown);
    return () => { sheet.removeEventListener("keydown", handleKeyDown); previousFocusRef.current?.isConnected && previousFocusRef.current.focus(); };
  }, []);
  return <><button className="sidebar-scrim" type="button" aria-label="Close panel" onClick={onClose} /><aside ref={sheetRef} className={`${className} is-open`} role="dialog" aria-modal="true" aria-labelledby={labelledBy} tabIndex={-1}>{children}</aside></>;
}

export function CalendarSidebar({ calendars, selected, onToggle, onCreate, onRefresh, refreshDisabled, refreshing, onClose, onJumpToDate, showClose = true, visibleDate = new Date() }: { calendars: CalendarRecord[]; selected: string[]; onToggle: (calendar: CalendarRecord) => void; onCreate: () => void; onRefresh: () => void; refreshDisabled: boolean; refreshing: boolean; onClose: () => void; onJumpToDate: (date: Date) => void; showClose?: boolean; visibleDate?: Date }) {
  const sourceLabel = (calendar: CalendarRecord) => calendar.accountLabel ?? providerLabel(calendar.provider);
  const groups = useMemo(() => Array.from(new Set(calendars.map(sourceLabel))), [calendars]);
  const today = new Date();
  const year = visibleDate.getFullYear();
  const month = visibleDate.getMonth();
  const firstWeekday = (new Date(year, month, 1).getDay() + 6) % 7;
  const daysInMonth = new Date(year, month + 1, 0).getDate();
  const miniDays = Array.from({ length: 42 }, (_, index) => {
    const day = index - firstWeekday + 1;
    return day > 0 && day <= daysInMonth ? day : null;
  });
  return <><div className="sidebar-heading"><h1 id="calendar-filters-title"><CalendarDays size={22} /> Calendar</h1>{showClose && <button className="icon-button" onClick={onClose} aria-label="Hide calendar filters"><X size={18} /></button>}</div><button className="button button-primary new-event-button" onClick={onCreate}><Plus size={17} /> New event</button><div className="mini-calendar"><div className="mini-calendar-title"><strong>{new Intl.DateTimeFormat(undefined, { month: "long", year: "numeric" }).format(visibleDate)}</strong></div><div className="mini-weekdays">{["M", "T", "W", "T", "F", "S", "S"].map((day, index) => <span key={`${day}-${index}`}>{day}</span>)}</div><div className="mini-days">{miniDays.map((day, index) => day ? <button type="button" key={index} className={day === today.getDate() && month === today.getMonth() && year === today.getFullYear() ? "is-today" : ""} aria-current={day === today.getDate() && month === today.getMonth() && year === today.getFullYear() ? "date" : undefined} onClick={() => onJumpToDate(new Date(year, month, day))} aria-label={`Go to ${new Intl.DateTimeFormat(undefined, { dateStyle: "long" }).format(new Date(year, month, day))}`}>{day}</button> : <span key={index} />)}</div></div><div className="calendar-list"><div className="section-label">Calendars <span>{selected.length}/{calendars.length}</span></div>{groups.map((group) => <div className="calendar-group" key={group}><div className="group-title">{group}</div>{calendars.filter((calendar) => sourceLabel(calendar) === group).map((calendar) => { const identity = `${calendar.name} (${sourceLabel(calendar)})`; return <label className={`calendar-filter ${calendar.capability.read ? "" : "is-disabled"}`} key={`${calendar.id}:${calendar.accountLabel ?? calendar.provider}`} title={identity}><input type="checkbox" checked={selected.includes(calendar.id)} onChange={() => onToggle(calendar)} disabled={!calendar.capability.read} aria-label={`${identity} calendar`} /><span className="checkmark" style={{ backgroundColor: calendar.color }} /><span className="calendar-name" title={identity}>{calendar.name}</span></label>; })}</div>)}</div><div className="sidebar-footer"><button className="button button-outline" onClick={onRefresh} disabled={refreshDisabled} aria-label="Refresh selected calendars"><RefreshCw size={16} className={refreshing ? "spin" : undefined} /> Refresh</button><div className="sidebar-manage"><ManageMenu /></div></div></>;
}

export function ManageMenu() {
  const [open, setOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const close = (restoreFocus = true) => {
    setOpen(false);
    if (restoreFocus) requestAnimationFrame(() => triggerRef.current?.focus());
  };

  useEffect(() => {
    if (!open) return;
    const handlePointerDown = (event: PointerEvent) => {
      if (menuRef.current?.contains(event.target as Node)) return;
      close(false);
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") { event.preventDefault(); close(); }
    };
    document.addEventListener("pointerdown", handlePointerDown);
    document.addEventListener("keydown", handleKeyDown);
    return () => { document.removeEventListener("pointerdown", handlePointerDown); document.removeEventListener("keydown", handleKeyDown); };
  }, [open]);

  return <div className="manage-menu" ref={menuRef}><button ref={triggerRef} type="button" className="button button-outline manage-menu-trigger" aria-haspopup="menu" aria-expanded={open} aria-controls="calendar-manage-menu" onClick={() => setOpen((current) => !current)}><MoreHorizontal size={17} /> Manage</button>{open && <div id="calendar-manage-menu" className="manage-menu-popover" role="menu" aria-label="Manage calendar"><NavLink to="/connections" role="menuitem" onClick={() => close()}>Connections</NavLink><NavLink to="/rules" role="menuitem" onClick={() => close()}>Sync activity</NavLink><NavLink to="/settings" role="menuitem" onClick={() => close()}>Settings</NavLink></div>}</div>;
}

function providerLabel(provider: string) { return provider === "microsoft" ? "Microsoft 365" : provider === "google" ? "Google" : provider === "apple" ? "Apple" : provider; }

export function shouldShowDetailedEventContent(start: Date | null, end: Date | null, allDay: boolean): boolean {
  return !allDay && Boolean(start && end && end.getTime() - start.getTime() >= 60 * 60 * 1000);
}

function MobileMonth({ visibleDate, days, calendars, onNavigate, onSelectDay, onSelectEvent, onCreate, onFilters }: { visibleDate: Date; days: MonthDay[]; calendars: Map<string, CalendarRecord>; onNavigate: (direction: "prev" | "next" | "today") => void; onSelectDay: (date: Date) => void; onSelectEvent: (event: EventRecord) => void; onCreate: () => void; onFilters: () => void }) {
  const monthLabel = new Intl.DateTimeFormat(undefined, { month: "long" }).format(visibleDate);
  const year = new Intl.DateTimeFormat(undefined, { year: "numeric" }).format(visibleDate);
  const todayKey = calendarDayKey(new Date());
  return <section className="mobile-month" aria-label={`${monthLabel} ${year}`}>
    <header className="mobile-month-header"><button className="button button-outline" onClick={() => onNavigate("prev")} aria-label="Previous month"><ChevronLeft size={19} /> {year}</button><div className="mobile-month-actions"><button className="icon-button bordered" onClick={onFilters} aria-label="Choose calendars"><CalendarDays size={18} /></button><button className="icon-button bordered" onClick={onCreate} aria-label="New event"><Plus size={20} /></button></div></header>
    <h1>{monthLabel}</h1><div className="mobile-month-weekdays">{["M", "T", "W", "T", "F", "S", "S"].map((day, index) => <span key={`${day}-${index}`}>{day}</span>)}</div>
    <div className="mobile-month-grid">{Array.from({ length: 6 }, (_, row) => <div className="mobile-month-row" key={row}>{days.slice(row * 7, row * 7 + 7).map((day) => {
      const inMonth = day.date.getMonth() === visibleDate.getMonth();
      const visibleEvents = day.events.slice(0, MOBILE_MONTH_EVENT_LIMIT);
      const overflow = day.events.length - visibleEvents.length;
      if (!inMonth) return <div className="mobile-month-day is-outside" key={day.key} aria-hidden="true" />;
      const dateLabel = new Intl.DateTimeFormat(undefined, { dateStyle: "long" }).format(day.date);
      return <div className={`mobile-month-day ${day.key === todayKey ? "is-today" : ""}`} key={day.key} role="gridcell" aria-label={`${dateLabel}${day.events.length ? `, ${day.events.length} events` : ""}`}><button type="button" className="mobile-month-day-number" onClick={() => onSelectDay(day.date)} aria-label={`Open ${dateLabel}`}>{day.date.getDate()}</button>{visibleEvents.map((event) => <button type="button" key={`${event.calendarId}:${event.id}`} className="mobile-month-event" style={{ "--event-color": calendars.get(event.calendarId)?.color ?? "#4762ee" } as React.CSSProperties} onClick={() => onSelectEvent(event)} title={event.title || "Untitled event"}>{event.title || "Untitled event"}</button>)}{overflow > 0 && <button type="button" className="mobile-month-more" onClick={() => onSelectDay(day.date)} aria-label={`Open ${overflow} more events on ${dateLabel}`}>+{overflow}</button>}</div>;
    })}</div>)}</div>
    <footer className="mobile-month-footer"><button className="button button-outline" onClick={() => onNavigate("today")}>Today</button><button className="mobile-month-next" onClick={() => onNavigate("next")}>Next month <ChevronRight size={18} /></button><button className="button button-outline" onClick={onFilters}>Calendars</button></footer>
  </section>;
}

function CalendarEventContent({ arg }: { arg: EventContentArg }) {
  const event = arg.event.extendedProps.event as EventRecord | undefined;
  const detailed = shouldShowDetailedEventContent(arg.event.start, arg.event.end, arg.event.allDay);
  return <div className={`fc-event-inner ${detailed ? "is-detailed" : ""}`}><strong>{arg.timeText && <span className="fc-event-time-label">{arg.timeText}</span>}{arg.event.title}</strong>{event?.location && <span className="fc-event-location">{event.location}</span>}</div>;
}

function EventDrawer({ event, calendar, onClose, onEdit, onDelete }: { event: EventRecord; calendar?: CalendarRecord; onClose: () => void; onEdit: () => void; onDelete: () => void }) {
  const editable = !event.readOnly && Boolean(calendar?.capability.write);
  const calendarIdentity = calendar ? `${calendar.name} (${calendar.accountLabel ?? providerLabel(calendar.provider)})` : event.calendarId;
  const videoURL = event.conference?.entry_points?.find((entry) => entry.type === "video" && isExternalURL(entry.uri))?.uri;
  return <><div className="drawer-header"><span id="event-details-title">Event details</span><button className="icon-button" onClick={onClose} aria-label="Close event details"><X size={20} /></button></div><div className="drawer-content"><div className="drawer-title-row"><h2>{event.title || "Untitled event"}</h2></div><div className="event-source"><span className="source-swatch" style={{ backgroundColor: calendar?.color }} /><span>{calendar?.name ?? event.calendarId}</span><span className="source-provider">{calendar?.accountLabel ?? providerLabel(calendar?.provider ?? event.source ?? "calendar")}</span>{event.readOnly && <span className="readonly-label">Read only</span>}</div><div className="drawer-divider" /><div className="detail-row"><Clock3 className="detail-icon" size={18} /><div><strong>{formatEventDate(event)}</strong><span>{formatEventTime(event)}</span>{event.recurrence?.isRecurring && <span>Repeats</span>}</div></div>{event.location && <div className="detail-row"><MapPin className="detail-icon" size={18} /><div><strong>Location</strong><span>{event.location}</span></div></div>}{videoURL && <div className="detail-row"><Video className="detail-icon" size={18} /><div><strong>{event.conference?.solution || "Video meeting"}</strong><a className="event-link" href={videoURL} target="_blank" rel="noreferrer">Join video meeting</a></div></div>}{event.description && <div className="detail-row"><AlignLeft className="detail-icon" size={18} /><div><strong>Description</strong>{event.descriptionFormat === "html" ? <HtmlDescription html={event.description} /> : <span className="description-text">{event.description}</span>}</div></div>}{event.htmlLink && isExternalURL(event.htmlLink) && <div className="drawer-meta"><a className="event-link" href={event.htmlLink} target="_blank" rel="noreferrer">Open in calendar</a></div>}<div className="drawer-meta"><span>Calendar</span><strong title={calendarIdentity}>{calendarIdentity}</strong></div></div><div className="drawer-actions"><button className="button button-outline" disabled={!editable} onClick={onEdit}>Edit</button><button className="button button-danger" disabled={!editable || !calendar?.capability.delete} onClick={onDelete}>Delete</button></div></>;
}

function isExternalURL(value: string): boolean {
  try { return ["http:", "https:"].includes(new URL(value).protocol); } catch { return false; }
}

function Dialog({ children, onClose, labelledBy, role = "dialog", className = "" }: { children: ReactNode; onClose: () => void; labelledBy: string; role?: "dialog" | "alertdialog"; className?: string }) {
  const dialogRef = useRef<HTMLElement>(null);
  const previousFocusRef = useRef<HTMLElement | null>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    previousFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const dialog = dialogRef.current;
    if (!dialog) return;
    const dialogElement: HTMLElement = dialog;
    const focusable = () => Array.from(dialogElement.querySelectorAll<HTMLElement>("button, [href], input, select, textarea, [tabindex]:not([tabindex='-1'])")).filter((element) => !element.hasAttribute("disabled") && element.getAttribute("aria-hidden") !== "true");
    const first = focusable()[0];
    (first ?? dialogElement).focus();
    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") {
        event.preventDefault();
        onCloseRef.current();
        return;
      }
      if (event.key !== "Tab") return;
      const elements = focusable();
      if (!elements.length) {
        event.preventDefault();
        dialogElement.focus();
        return;
      }
      const current = document.activeElement;
      const firstElement = elements[0];
      const lastElement = elements[elements.length - 1];
      if (event.shiftKey && current === firstElement) {
        event.preventDefault();
        lastElement.focus();
      } else if (!event.shiftKey && current === lastElement) {
        event.preventDefault();
        firstElement.focus();
      }
    }
    dialogElement.addEventListener("keydown", handleKeyDown);
    return () => {
      dialogElement.removeEventListener("keydown", handleKeyDown);
      if (previousFocusRef.current?.isConnected) previousFocusRef.current.focus();
    };
  }, []);

  return <div className="modal-backdrop" role="presentation"><section ref={dialogRef} className={`modal ${className}`} role={role} aria-modal="true" aria-labelledby={labelledBy} tabIndex={-1}>{children}</section></div>;
}

function EventModal({ mode, event, preset, calendars, onClose, onSave, busy }: { mode: "create" | "edit"; event?: EventRecord; preset?: Partial<EventDraft>; calendars: CalendarRecord[]; onClose: () => void; onSave: (draft: EventDraft, calendarId: string) => void; busy: boolean }) {
  const initial = { ...toEventDraft(event), ...preset };
  const [draft, setDraft] = useState<EventDraft>(initial);
  const [calendarId, setCalendarId] = useState(event?.calendarId ?? calendars.find((calendar) => calendar.capability.create)?.id ?? "");
  const writableCalendars = calendars.filter((calendar) => (event ? calendar.capability.write : calendar.capability.create) && !calendar.readOnly);
  function update<K extends keyof EventDraft>(key: K, value: EventDraft[K]) { setDraft((current) => ({ ...current, [key]: value })); }
  function changeAllDay(allDay: boolean) { setDraft((current) => toggleAllDayDraft(current, allDay)); }
  const valid = draft.title.trim().length > 0 && draft.start && draft.end && new Date(draft.end).getTime() > new Date(draft.start).getTime();
  return <Dialog onClose={onClose} labelledBy="event-modal-title" className="event-modal"><div className="modal-header"><h2 id="event-modal-title">{mode === "create" ? "New event" : "Edit event"}</h2><button className="icon-button" onClick={onClose} aria-label="Close dialog"><X size={20} /></button></div><form onSubmit={(e) => { e.preventDefault(); if (valid && calendarId) onSave(draft, calendarId); }}><label className="field"><span>Title</span><input value={draft.title} onChange={(e) => update("title", e.target.value)} placeholder="Add a title" /></label><label className="field"><span>Calendar</span><select value={calendarId} onChange={(e) => setCalendarId(e.target.value)} disabled={mode === "edit"}>{writableCalendars.length ? writableCalendars.map((calendar) => <option key={calendar.id} value={calendar.id}>{calendar.name} ({providerLabel(calendar.provider)})</option>) : <option value="">No writable calendars</option>}</select></label><label className="toggle-field"><span>All day</span><input type="checkbox" checked={draft.allDay} onChange={(e) => changeAllDay(e.target.checked)} /><span className="toggle" /></label><div className="field-row"><label className="field"><span>Starts</span><input type={draft.allDay ? "date" : "datetime-local"} value={draft.start} onChange={(e) => update("start", e.target.value)} /></label><label className="field"><span>Ends</span><input type={draft.allDay ? "date" : "datetime-local"} value={draft.end} onChange={(e) => update("end", e.target.value)} /></label></div><label className="field"><span>Location <em>Optional</em></span><input value={draft.location} onChange={(e) => update("location", e.target.value)} placeholder="Add a location" /></label><label className="field"><span>Description <em>Optional</em></span><textarea value={draft.description} onChange={(e) => update("description", e.target.value)} placeholder="Add a description" rows={4} /></label><div className="modal-actions"><button type="button" className="button button-secondary" onClick={onClose}>Cancel</button><button type="submit" className="button button-primary" disabled={!valid || !calendarId || busy}>{busy ? "Saving…" : mode === "create" ? "Create event" : "Save changes"}</button></div></form></Dialog>;
}

function ScopeDialog({ event, action, scopes: supportedScopes, onCancel, onConfirm }: { event: EventRecord; action: "reschedule" | "edit" | "delete"; scopes?: Array<"single" | "following" | "series">; onCancel: () => void; onConfirm: (scope: "single" | "following" | "series") => void }) {
  const scopes = supportedScopes?.length ? supportedScopes : event.recurrence?.scopes ?? ["single", "following", "series"];
  const labels = { single: "This event", following: "This and following", series: "Entire series" };
  return <Dialog onClose={onCancel} labelledBy="scope-title" className="scope-modal"><div className="modal-header"><h2 id="scope-title">Choose what to {action === "delete" ? "delete" : "change"}</h2><button className="icon-button" onClick={onCancel} aria-label="Close dialog"><X size={20} /></button></div><p className="modal-intro">“{event.title}” repeats. Select the occurrences this action should apply to.</p><div className="scope-options">{scopes.map((scope) => <button key={scope} className="scope-option" onClick={() => onConfirm(scope)}><span>{labels[scope]}</span><ChevronRight size={17} /></button>)}</div><button className="button button-secondary full-width" onClick={onCancel}>Cancel</button></Dialog>;
}

function ConfirmDialog({ event, busy, onCancel, onConfirm }: { event: EventRecord; busy: boolean; onCancel: () => void; onConfirm: () => void }) {
  return <Dialog onClose={onCancel} labelledBy="delete-title" role="alertdialog" className="scope-modal"><div className="modal-header"><h2 id="delete-title">Delete event?</h2><button className="icon-button" onClick={onCancel} aria-label="Close dialog"><X size={20} /></button></div><p className="modal-intro">This will permanently remove “{event.title}” from {event.calendarId}. This action cannot be undone.</p><div className="modal-actions"><button className="button button-secondary" onClick={onCancel}>Cancel</button><button className="button button-danger" onClick={onConfirm} disabled={busy}>{busy ? "Deleting…" : "Delete event"}</button></div></Dialog>;
}
