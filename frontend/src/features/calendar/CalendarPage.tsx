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
import { AlignLeft, CalendarDays, ChevronLeft, ChevronRight, Clock3, Link2, ListChecks, ListFilter, MapPin, PlayCircle, Plus, RefreshCw, Settings2, X } from "lucide-react";
import { NavLink } from "react-router-dom";
import { useBootstrapData } from "../../app/App";
import { APIError, createEvent, deleteEvent, getEvents, refreshCalendar, updateEvent } from "../../lib/api";
import { EVENT_STATUS_POLL_MAX_ATTEMPTS, calendarEventKey, canWriteEvent, eventStatusPollInterval, formatEventDate, formatEventTime, selectedReadableCalendarIds, sortCalendarIds, summarizeEventSources, toCalendarEvent, toCreatePayload, toEventDraft, toLocalInputValue, toReschedulePayload, toUpdatePayload, toggleAllDayDraft, withMutationScope } from "../../lib/calendar";
import type { CalendarRecord, EventDraft, EventRecord } from "../../lib/types";
import { ErrorState, EmptyState, LoadingState } from "../../components/AsyncState";

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

const CALENDAR_VIEW_STORAGE_KEY = "calendar:view";
const COMPACT_LAYOUT_QUERY = "(max-width: 1080px)";

const VIEW_OPTIONS: Array<{ value: ViewName; label: string }> = [
  { value: "timeGridDay", label: "Day" },
  { value: "timeGridWeek", label: "Week" },
  { value: "dayGridMonth", label: "Month" },
  { value: "listWeek", label: "List" },
];

export default function CalendarPage() {
  const { csrf_token: csrfToken, calendars } = useBootstrapData();
  const calendarRef = useRef<FullCalendar>(null);
  const queryClient = useQueryClient();
  const [range, setRange] = useState<Range>(null);
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
  const [surface, setSurface] = useState<Surface>({ kind: "none" });
  const scopePayloadRef = useRef<ScopePayload | null>(null);
  const [editEvent, setEditEvent] = useState<EventRecord | null>(null);
  const [notice, setNotice] = useState<Notice>(null);
  const [deleteConfirm, setDeleteConfirm] = useState<EventRecord | null>(null);
  const lastPolledDataUpdatedAtRef = useRef(0);
  const [pollingAttempts, setPollingAttempts] = useState(0);
  const [pollingCapped, setPollingCapped] = useState(false);
  const sortedSelectedCalendarIds = useMemo(() => sortCalendarIds(selectedCalendarIds), [selectedCalendarIds]);

  const eventsQuery = useQuery({
    queryKey: ["events", range?.start, range?.end, sortedSelectedCalendarIds],
    queryFn: () => getEvents(range!.start, range!.end, sortedSelectedCalendarIds),
    enabled: Boolean(range && sortedSelectedCalendarIds.length),
    placeholderData: (previous) => previous,
    refetchInterval: (query) => {
      return eventStatusPollInterval(query.state.data?.sources, pollingAttempts);
    },
    refetchIntervalInBackground: false,
  });
  const events = eventsQuery.data?.items ?? [];
  const baseEventStatus = eventsQuery.data ? summarizeEventSources(eventsQuery.data) : null;
  const eventStatus = baseEventStatus?.kind === "syncing" && pollingCapped ? { ...baseEventStatus, kind: "stale" as const, label: "Sync paused; refresh to try again" } : baseEventStatus;
  const hasPendingSources = eventsQuery.data?.sources?.some((source) => source.status === "pending" || source.status === "syncing") ?? false;
  const eventsById = useMemo(() => new Map(events.map((event) => [calendarEventKey(event), event])), [events]);
  const calendarById = useMemo(() => new Map(calendars.map((calendar) => [calendar.id, calendar])), [calendars]);
  const calendarEvents = useMemo(() => events.map((event) => toCalendarEvent(event, calendarById.get(event.calendarId))), [events, calendarById]);

  const invalidateEvents = () => void queryClient.invalidateQueries({ queryKey: ["events"] });
  useEffect(() => {
    setPollingAttempts(0);
    setPollingCapped(false);
    lastPolledDataUpdatedAtRef.current = 0;
  }, [range?.start, range?.end, sortedSelectedCalendarIds]);
  const refreshMutation = useMutation({
    mutationFn: (calendarIds: string[]) => Promise.all(calendarIds.map((calendarId) => refreshCalendar(csrfToken, calendarId))),
    onSuccess: () => setNotice({ message: "Calendar refresh queued", intent: "success" }),
    onError: (error) => setNotice({ message: safeError(error), intent: "error" }),
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
  const createMutation = useMutation({
    mutationFn: (payload: Parameters<typeof createEvent>[1]) => createEvent(csrfToken, payload),
    onSuccess: (event) => { setSurface({ kind: "none" }); setNotice(mutationNotice("Event created", event.warnings)); invalidateEvents(); },
    onError: (error) => setNotice({ message: safeError(error), intent: "error" }),
  });
  const updateMutation = useMutation({
    mutationFn: (args: { event: EventRecord; payload: Parameters<typeof updateEvent>[3]; revert?: () => void }) => updateEvent(csrfToken, args.event.calendarId, args.event.id, args.payload),
    onSuccess: (event) => { setSurface((current) => current.kind === "event" ? { kind: "event", event } : current); setEditEvent(null); setNotice(mutationNotice("Event updated", event.warnings)); invalidateEvents(); },
    onError: (error, variables) => { variables.revert?.(); if (error instanceof APIError && error.status === 409) { setNotice({ message: "This event changed elsewhere. We refreshed it; please try again.", intent: "error" }); void queryClient.invalidateQueries({ queryKey: ["events"] }); } else setNotice({ message: safeError(error), intent: "error" }); },
  });
  const deleteMutation = useMutation({
    mutationFn: (args: { event: EventRecord; scope?: "single" | "following" | "series" }) => deleteEvent(csrfToken, args.event.calendarId, args.event.id, { scope: args.scope ?? "single", expected_etag: args.event.etag, effective_from: args.scope === "following" ? args.event.originalStart : undefined }),
    onSuccess: (result) => { scopePayloadRef.current = null; setSurface({ kind: "none" }); setNotice(mutationNotice("Event deleted", result.warnings)); invalidateEvents(); },
    onError: (error) => setNotice({ message: safeError(error), intent: "error" }),
  });

  function changeView(nextView: ViewName) {
    setView(nextView);
    try { localStorage.setItem(CALENDAR_VIEW_STORAGE_KEY, nextView); } catch { /* persistence is optional */ }
    calendarRef.current?.getApi().changeView(nextView);
  }
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
    else setDeleteConfirm(event);
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
    {persistentSidebar && <aside className="calendar-sidebar is-open" aria-label="Calendar filters"><CalendarSidebar showClose={false} calendars={calendars} selected={selectedCalendarIds} onToggle={toggleCalendar} onCreate={() => openCreate()} onClose={() => undefined} onJumpToDate={(date) => { calendarRef.current?.getApi().gotoDate(date); }} /></aside>}
    {surface.kind === "filters" && compactLayout && <SurfaceSheet className="calendar-sidebar" labelledBy="calendar-filters-title" onClose={() => setSurface({ kind: "none" })}><CalendarSidebar calendars={calendars} selected={selectedCalendarIds} onToggle={toggleCalendar} onCreate={() => openCreate()} onClose={() => setSurface({ kind: "none" })} onJumpToDate={(date) => { calendarRef.current?.getApi().gotoDate(date); setSurface({ kind: "none" }); }} /></SurfaceSheet>}
    <main className="calendar-main" inert={sheetOpen} aria-hidden={sheetOpen || undefined}>
      <div className="calendar-toolbar">
        <div className="toolbar-primary"><button className="button button-outline today-button" onClick={() => navigate("today")}>Today</button><div className="nav-arrows"><button className="icon-button bordered" onClick={() => navigate("prev")} aria-label="Previous period"><ChevronLeft size={19} /></button><button className="icon-button bordered" onClick={() => navigate("next")} aria-label="Next period"><ChevronRight size={19} /></button></div><span className="date-title" aria-live="polite">{calendarRef.current?.getApi().view.title ?? "Calendar"}</span></div>
        <div className="toolbar-actions"><button className="button button-primary" onClick={() => openCreate()}><Plus size={16} /> New event</button><button className="button button-outline" onClick={() => setSurface({ kind: "filters" })} aria-label="Choose calendars"><ListFilter size={15} /> Calendars</button><button className="button button-outline" onClick={refreshSelectedCalendars} disabled={refreshMutation.isPending || !sortedSelectedCalendarIds.length} aria-label="Refresh selected calendars"><RefreshCw size={15} className={refreshMutation.isPending ? "spin" : undefined} /> Refresh</button><div className="view-switcher" role="group" aria-label="Calendar view">{VIEW_OPTIONS.map((option) => <button key={option.value} className={view === option.value ? "is-selected" : ""} aria-pressed={view === option.value} onClick={() => changeView(option.value)}>{option.label}</button>)}</div></div>
      </div>
      <div className="calendar-mobile-filter"><button className="button button-outline" onClick={() => setSurface({ kind: "filters" })}><ListFilter size={16} /> Calendars</button><button className="button button-primary" onClick={() => openCreate()}><Plus size={16} /> New event</button></div>
      {eventStatus && <div className={`event-status-strip event-status-row event-status-row--${eventStatus.kind}`} role={eventStatus.kind === "failed" ? "alert" : "status"} aria-live="polite"><span>{eventStatus.label}</span>{(eventStatus.kind === "failed" || eventStatus.kind === "degraded") && <span className="event-status-detail">Cached events remain visible.</span>}{eventStatus.kind === "degraded" && <button className="status-action" type="button" onClick={refreshDegradedCalendars} disabled={refreshMutation.isPending}>Refresh affected calendars</button>}</div>}
      {notice && <div className={`notice notice--${notice.intent}`} role={notice.intent === "error" ? "alert" : "status"}><span>{notice.message}</span><button className="icon-button" onClick={() => setNotice(null)} aria-label="Dismiss notice"><X size={16} /></button></div>}
      {!selectedCalendarIds.length ? <EmptyState title="No calendars selected" message="Choose at least one calendar from the sidebar to see your events." action={<button className="button button-secondary" onClick={showAllCalendars}>Show all calendars</button>} /> : <div className="calendar-canvas-wrap">
        {eventsQuery.isPending && !eventsQuery.data && <div className="calendar-overlay"><LoadingState label="Loading events" /></div>}
        {eventsQuery.isError && !events.length && <div className="calendar-overlay"><ErrorState message={safeError(eventsQuery.error)} retry={() => void eventsQuery.refetch()} /></div>}
        <FullCalendar ref={calendarRef} plugins={[dayGridPlugin, timeGridPlugin, listPlugin, interactionPlugin]} initialView={view} headerToolbar={false} height="100%" expandRows selectable selectMirror editable eventStartEditable eventDurationEditable nowIndicator dayMaxEvents={3} events={calendarEvents} select={handleSelect} eventClick={handleEventClick} eventDrop={handleMove} eventResize={handleMove} datesSet={(arg: DatesSetArg) => setRange({ start: arg.start.toISOString(), end: arg.end.toISOString() })} eventContent={(arg) => <CalendarEventContent arg={arg} />} />
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

function CalendarSidebar({ calendars, selected, onToggle, onCreate, onClose, onJumpToDate, showClose = true }: { calendars: CalendarRecord[]; selected: string[]; onToggle: (calendar: CalendarRecord) => void; onCreate: () => void; onClose: () => void; onJumpToDate: (date: Date) => void; showClose?: boolean }) {
  const groups = useMemo(() => Array.from(new Set(calendars.map((calendar) => calendar.group || "Calendars"))), [calendars]);
  const navigation = [{ to: "/app", label: "Calendar", icon: CalendarDays }, { to: "/connections", label: "Connections", icon: Link2 }, { to: "/rules", label: "Sync Rules", icon: ListChecks }, { to: "/runs", label: "Runs", icon: PlayCircle }, { to: "/settings", label: "Settings", icon: Settings2 }];
  const today = new Date();
  const year = today.getFullYear();
  const month = today.getMonth();
  const firstWeekday = new Date(year, month, 1).getDay();
  const daysInMonth = new Date(year, month + 1, 0).getDate();
  const miniDays = Array.from({ length: 42 }, (_, index) => {
    const day = index - firstWeekday + 1;
    return day > 0 && day <= daysInMonth ? day : null;
  });
  return <><div className="sidebar-heading"><h1 id="calendar-filters-title"><CalendarDays size={22} /> Calendar</h1>{showClose && <button className="icon-button" onClick={onClose} aria-label="Hide calendar filters"><X size={18} /></button>}</div><nav className="calendar-navigation" aria-label="Primary navigation">{navigation.map(({ to, label, icon: Icon }) => <NavLink key={to} to={to} end={to === "/app"} className={({ isActive }) => `calendar-navigation-link ${isActive ? "is-active" : ""}`}><Icon size={17} /><span>{label}</span></NavLink>)}</nav><button className="button button-primary new-event-button" onClick={onCreate}><Plus size={17} /> New event</button><div className="mini-calendar"><div className="mini-calendar-title"><strong>{new Intl.DateTimeFormat(undefined, { month: "long", year: "numeric" }).format(today)}</strong></div><div className="mini-weekdays">{["S", "M", "T", "W", "T", "F", "S"].map((day, index) => <span key={`${day}-${index}`}>{day}</span>)}</div><div className="mini-days">{miniDays.map((day, index) => day ? <button type="button" key={index} className={day === today.getDate() ? "is-today" : ""} aria-current={day === today.getDate() ? "date" : undefined} onClick={() => onJumpToDate(new Date(year, month, day))} aria-label={`Go to ${new Intl.DateTimeFormat(undefined, { dateStyle: "long" }).format(new Date(year, month, day))}`}>{day}</button> : <span key={index} />)}</div></div><div className="calendar-list"><div className="section-label">Calendars <span>{selected.length}/{calendars.length}</span></div>{groups.map((group) => <div className="calendar-group" key={group}><div className="group-title">{group}</div>{calendars.filter((calendar) => (calendar.group || "Calendars") === group).map((calendar) => <label className={`calendar-filter ${calendar.capability.read ? "" : "is-disabled"}`} key={calendar.id} title={calendar.name}><input type="checkbox" checked={selected.includes(calendar.id)} onChange={() => onToggle(calendar)} disabled={!calendar.capability.read} aria-label={`${calendar.name} calendar`} /><span className="checkmark" style={{ backgroundColor: calendar.color }} /><span className="calendar-name" title={calendar.name}>{calendar.name}</span><span className="calendar-provider">{providerLabel(calendar.provider)}</span></label>)}</div>)}</div></>;
}

function providerLabel(provider: string) { return provider === "microsoft" ? "Microsoft 365" : provider === "google" ? "Google" : provider === "apple" ? "Apple" : provider; }

function CalendarEventContent({ arg }: { arg: EventContentArg }) {
  const event = arg.event.extendedProps.event as EventRecord | undefined;
  return <div className="fc-event-inner"><strong>{arg.timeText && <span className="fc-event-time-label">{arg.timeText}</span>}{arg.event.title}</strong>{event?.location && <span className="fc-event-location">{event.location}</span>}</div>;
}

function EventDrawer({ event, calendar, onClose, onEdit, onDelete }: { event: EventRecord; calendar?: CalendarRecord; onClose: () => void; onEdit: () => void; onDelete: () => void }) {
  const editable = !event.readOnly && Boolean(calendar?.capability.write);
  return <><div className="drawer-header"><span id="event-details-title">Event details</span><button className="icon-button" onClick={onClose} aria-label="Close event details"><X size={20} /></button></div><div className="drawer-content"><div className="drawer-title-row"><h2>{event.title || "Untitled event"}</h2></div><div className="event-source"><span className="source-swatch" style={{ backgroundColor: calendar?.color }} /><span>{calendar?.name ?? event.calendarId}</span><span className="source-provider">{providerLabel(calendar?.provider ?? event.source ?? "calendar")}</span>{event.readOnly && <span className="readonly-label">Read only</span>}</div><div className="drawer-divider" /><div className="detail-row"><Clock3 className="detail-icon" size={18} /><div><strong>{formatEventDate(event)}</strong><span>{formatEventTime(event)}</span>{event.recurrence?.isRecurring && <span>Repeats</span>}</div></div>{event.location && <div className="detail-row"><MapPin className="detail-icon" size={18} /><div><strong>Location</strong><span>{event.location}</span></div></div>}{event.description && <div className="detail-row"><AlignLeft className="detail-icon" size={18} /><div><strong>Description</strong><span className="description-text">{event.description}</span></div></div>}<div className="drawer-meta"><span>Calendar</span><strong>{calendar?.name ?? event.calendarId}</strong></div></div><div className="drawer-actions"><button className="button button-outline" disabled={!editable} onClick={onEdit}>Edit</button><button className="button button-danger" disabled={!editable || !calendar?.capability.delete} onClick={onDelete}>Delete</button></div></>;
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
