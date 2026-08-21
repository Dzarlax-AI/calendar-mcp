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
import { AlignLeft, CalendarDays, ChevronDown, ChevronLeft, ChevronRight, CircleAlert, Clock3, Link2, ListChecks, ListFilter, MapPin, PlayCircle, Plus, Settings2, X } from "lucide-react";
import { NavLink } from "react-router-dom";
import { useBootstrapData } from "../../app/App";
import { APIError, createEvent, deleteEvent, getEvents, updateEvent } from "../../lib/api";
import { calendarEventKey, canWriteEvent, formatEventDate, formatEventTime, selectedReadableCalendarIds, sortCalendarIds, toCalendarEvent, toCreatePayload, toEventDraft, toLocalInputValue, toReschedulePayload, toUpdatePayload, toggleAllDayDraft, withMutationScope } from "../../lib/calendar";
import type { CalendarRecord, EventDraft, EventRecord } from "../../lib/types";
import { ErrorState, EmptyState, LoadingState } from "../../components/AsyncState";

type ViewName = "timeGridDay" | "timeGridWeek" | "dayGridMonth" | "listWeek";
type Range = { start: string; end: string } | null;
type ModalState = { mode: "create" | "edit"; event?: EventRecord; preset?: Partial<EventDraft> } | null;
type PendingScope = { event: EventRecord; action: "reschedule" | "edit" | "delete"; revert?: () => void; payload?: ReturnType<typeof toReschedulePayload> | ReturnType<typeof toUpdatePayload> } | null;

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
  const [view, setView] = useState<ViewName>(() => window.matchMedia?.("(max-width: 760px)").matches ? "timeGridDay" : "timeGridWeek");
  const [selectedCalendarIds, setSelectedCalendarIds] = useState<string[]>(() => {
    try {
      const saved = JSON.parse(localStorage.getItem("calendar:selected") ?? "null");
      const selected = selectedReadableCalendarIds(calendars, saved);
      if (Array.isArray(saved)) localStorage.setItem("calendar:selected", JSON.stringify(selected));
      return selected;
    } catch { /* use all calendars */ }
    return selectedReadableCalendarIds(calendars, null);
  });
  const [selectedEvent, setSelectedEvent] = useState<EventRecord | null>(null);
  const [modal, setModal] = useState<ModalState>(null);
  const [pendingScope, setPendingScope] = useState<PendingScope>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [sidebarOpen, setSidebarOpen] = useState(() => !window.matchMedia?.("(max-width: 820px)").matches);
  const [deleteConfirm, setDeleteConfirm] = useState<EventRecord | null>(null);
  const sortedSelectedCalendarIds = useMemo(() => sortCalendarIds(selectedCalendarIds), [selectedCalendarIds]);

  const eventsQuery = useQuery({
    queryKey: ["events", range?.start, range?.end, sortedSelectedCalendarIds],
    queryFn: () => getEvents(range!.start, range!.end, sortedSelectedCalendarIds),
    enabled: Boolean(range && sortedSelectedCalendarIds.length),
    placeholderData: (previous) => previous,
  });
  const events = eventsQuery.data?.items ?? [];
  const eventsById = useMemo(() => new Map(events.map((event) => [calendarEventKey(event), event])), [events]);
  const calendarById = useMemo(() => new Map(calendars.map((calendar) => [calendar.id, calendar])), [calendars]);
  const calendarEvents = useMemo(() => events.map((event) => toCalendarEvent(event, calendarById.get(event.calendarId))), [events, calendarById]);

  const invalidateEvents = () => void queryClient.invalidateQueries({ queryKey: ["events"] });
  const createMutation = useMutation({
    mutationFn: (payload: Parameters<typeof createEvent>[1]) => createEvent(csrfToken, payload),
    onSuccess: () => { setModal(null); setNotice("Event created"); invalidateEvents(); },
    onError: (error) => setNotice(safeError(error)),
  });
  const updateMutation = useMutation({
    mutationFn: (args: { event: EventRecord; payload: Parameters<typeof updateEvent>[3]; revert?: () => void }) => updateEvent(csrfToken, args.event.calendarId, args.event.id, args.payload),
    onSuccess: (event) => { setSelectedEvent(event); setModal(null); setNotice("Event updated"); invalidateEvents(); },
    onError: (error, variables) => { variables.revert?.(); if (error instanceof APIError && error.status === 409) { setNotice("This event changed elsewhere. We refreshed it; please try again."); void queryClient.invalidateQueries({ queryKey: ["events"] }); } else setNotice(safeError(error)); if (variables.event) setSelectedEvent(variables.event); },
  });
  const deleteMutation = useMutation({
    mutationFn: (args: { event: EventRecord; scope?: "single" | "following" | "series" }) => deleteEvent(csrfToken, args.event.calendarId, args.event.id, { scope: args.scope ?? "single", expected_etag: args.event.etag, effective_from: args.scope === "following" ? args.event.originalStart : undefined }),
    onSuccess: () => { setSelectedEvent(null); setPendingScope(null); setNotice("Event deleted"); invalidateEvents(); },
    onError: (error) => setNotice(safeError(error)),
  });

  function changeView(nextView: ViewName) {
    setView(nextView);
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
  function openCreate(preset?: Partial<EventDraft>) { setModal({ mode: "create", preset }); }
  function openEdit(event: EventRecord) {
    if (!canWriteEvent(event, calendarById.get(event.calendarId))) return;
    setModal({ mode: "edit", event });
  }
  function handleSelect(selection: DateSelectArg) {
    const start = selection.allDay ? selection.startStr : toLocalInputValue(selection.start.toISOString());
    const end = selection.allDay ? selection.endStr : toLocalInputValue(selection.end.toISOString());
    openCreate({ start, end, allDay: selection.allDay });
  }
  function handleEventClick(info: EventClickArg) {
    const event = eventsById.get(info.event.id) ?? info.event.extendedProps.event as EventRecord | undefined;
    if (event) setSelectedEvent(event);
  }
  function handleMove(info: EventDropArg | EventResizeDoneArg) {
    const event = eventsById.get(info.event.id) ?? info.event.extendedProps.event as EventRecord | undefined;
    if (!event || !canWriteEvent(event, calendarById.get(event.calendarId)) || !info.event.start || !info.event.end) { info.revert(); return; }
    const payload = toReschedulePayload(event, info.event.start, info.event.end);
    if (event.recurrence?.isRecurring) {
      setPendingScope({ event, action: "reschedule", revert: info.revert, payload });
    } else {
      updateMutation.mutate({ event, payload, revert: info.revert });
    }
  }
  function confirmScope(scope: "single" | "following" | "series") {
    if (!pendingScope) return;
    const { event, action, revert, payload } = pendingScope;
    setPendingScope(null);
    if (action === "delete") deleteMutation.mutate({ event, scope });
    else if ((action === "reschedule" || action === "edit") && payload) updateMutation.mutate({ event, payload: withMutationScope(event, payload, scope), revert });
  }
  function askDelete(event: EventRecord) {
    if (event.recurrence?.isRecurring) setPendingScope({ event, action: "delete" });
    else setDeleteConfirm(event);
  }
  function saveDraft(draft: EventDraft, calendarId: string) {
    if (modal?.mode === "create") createMutation.mutate(toCreatePayload(calendarId, draft));
    else if (modal?.event) {
      const payload = toUpdatePayload(modal.event, draft);
      if (modal.event.recurrence?.isRecurring) { setPendingScope({ event: modal.event, action: "edit", payload }); setModal(null); }
      else updateMutation.mutate({ event: modal.event, payload });
    }
  }

  return <div className={`calendar-screen ${sidebarOpen ? "with-sidebar" : "wide"} ${selectedEvent ? "with-drawer" : ""}`}>
    <CalendarSidebar calendars={calendars} selected={selectedCalendarIds} open={sidebarOpen} onToggle={toggleCalendar} onCreate={() => openCreate()} onClose={() => setSidebarOpen(false)} onJumpToDate={(date) => calendarRef.current?.getApi().gotoDate(date)} />
    <main className="calendar-main">
      <div className="calendar-toolbar">
        <div className="toolbar-primary"><button className="button button-outline today-button" onClick={() => navigate("today")}>Today</button><div className="nav-arrows"><button className="icon-button bordered" onClick={() => navigate("prev")} aria-label="Previous period"><ChevronLeft size={19} /></button><button className="icon-button bordered" onClick={() => navigate("next")} aria-label="Next period"><ChevronRight size={19} /></button></div><button className="date-title" onClick={() => navigate("today")} aria-label="Go to today">{calendarRef.current?.getApi().view.title ?? "Calendar"}<ChevronDown size={17} /></button></div>
        <div className="toolbar-actions"><div className="view-switcher" role="group" aria-label="Calendar view">{VIEW_OPTIONS.map((option) => <button key={option.value} className={view === option.value ? "is-selected" : ""} onClick={() => changeView(option.value)}>{option.label}</button>)}</div></div>
      </div>
      <div className="calendar-mobile-filter"><button className="button button-outline" onClick={() => setSidebarOpen(true)}><ListFilter size={16} /> Calendars</button><button className="button button-primary" onClick={() => openCreate()}><Plus size={16} /> New event</button></div>
      {notice && <div className="notice" role="status"><span>{notice}</span><button className="icon-button" onClick={() => setNotice(null)} aria-label="Dismiss notice"><X size={16} /></button></div>}
      {!selectedCalendarIds.length ? <EmptyState title="No calendars selected" message="Choose at least one calendar from the sidebar to see your events." action={<button className="button button-secondary" onClick={() => setSelectedCalendarIds(calendars.filter((calendar) => calendar.capability.read).map((calendar) => calendar.id))}>Show all calendars</button>} /> : <div className="calendar-canvas-wrap">
        {eventsQuery.isPending && <div className="calendar-overlay"><LoadingState label="Loading events" /></div>}
        {eventsQuery.isError && !events.length && <div className="calendar-overlay"><ErrorState message={safeError(eventsQuery.error)} retry={() => void eventsQuery.refetch()} /></div>}
        {eventsQuery.data && !eventsQuery.data.complete && <div className="partial-warning"><CircleAlert size={15} /> Some calendars could not finish loading.</div>}
        <FullCalendar ref={calendarRef} plugins={[dayGridPlugin, timeGridPlugin, listPlugin, interactionPlugin]} initialView={view} headerToolbar={false} height="100%" expandRows selectable selectMirror editable eventStartEditable eventDurationEditable nowIndicator dayMaxEvents={3} events={calendarEvents} select={handleSelect} eventClick={handleEventClick} eventDrop={handleMove} eventResize={handleMove} datesSet={(arg: DatesSetArg) => setRange({ start: arg.start.toISOString(), end: arg.end.toISOString() })} eventContent={(arg) => <CalendarEventContent arg={arg} />} />
      </div>}
    </main>
    {selectedEvent && <EventDrawer event={selectedEvent} calendar={calendarById.get(selectedEvent.calendarId)} onClose={() => setSelectedEvent(null)} onEdit={() => openEdit(selectedEvent)} onDelete={() => askDelete(selectedEvent)} />}
    {modal && <EventModal mode={modal.mode} event={modal.event} preset={modal.preset} calendars={calendars} onClose={() => setModal(null)} onSave={saveDraft} busy={createMutation.isPending || updateMutation.isPending} />}
    {pendingScope && <ScopeDialog event={pendingScope.event} action={pendingScope.action} scopes={calendarById.get(pendingScope.event.calendarId)?.capability.recurrenceScopes} onCancel={() => { pendingScope.revert?.(); setPendingScope(null); }} onConfirm={confirmScope} />}
    {deleteConfirm && <ConfirmDialog event={deleteConfirm} busy={deleteMutation.isPending} onCancel={() => setDeleteConfirm(null)} onConfirm={() => { const event = deleteConfirm; setDeleteConfirm(null); deleteMutation.mutate({ event }); }} />}
  </div>;
}

function safeError(error: unknown) { return error instanceof Error ? error.message : "The request could not be completed."; }

function CalendarSidebar({ calendars, selected, open, onToggle, onCreate, onClose, onJumpToDate }: { calendars: CalendarRecord[]; selected: string[]; open: boolean; onToggle: (calendar: CalendarRecord) => void; onCreate: () => void; onClose: () => void; onJumpToDate: (date: Date) => void }) {
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
  return <aside className={`calendar-sidebar ${open ? "is-open" : ""}`} aria-label="Calendar filters"><div className="sidebar-heading"><h1><CalendarDays size={22} /> Calendar</h1><button className="icon-button" onClick={onClose} aria-label="Hide calendar sidebar"><X size={18} /></button></div><nav className="calendar-navigation" aria-label="Primary navigation">{navigation.map(({ to, label, icon: Icon }) => <NavLink key={to} to={to} end={to === "/app"} className={({ isActive }) => `calendar-navigation-link ${isActive ? "is-active" : ""}`}><Icon size={17} /><span>{label}</span></NavLink>)}</nav><button className="button button-primary new-event-button" onClick={onCreate}><Plus size={17} /> New event</button><div className="mini-calendar"><div className="mini-calendar-title"><strong>{new Intl.DateTimeFormat(undefined, { month: "long", year: "numeric" }).format(today)}</strong></div><div className="mini-weekdays">{["S", "M", "T", "W", "T", "F", "S"].map((day, index) => <span key={`${day}-${index}`}>{day}</span>)}</div><div className="mini-days">{miniDays.map((day, index) => day ? <button type="button" key={index} className={day === today.getDate() ? "is-today" : ""} onClick={() => onJumpToDate(new Date(year, month, day))} aria-label={new Intl.DateTimeFormat(undefined, { dateStyle: "long" }).format(new Date(year, month, day))}>{day}</button> : <span key={index} />)}</div></div><div className="calendar-list"><div className="section-label">Calendars <span>{selected.length}/{calendars.length}</span></div>{groups.map((group) => <div className="calendar-group" key={group}><div className="group-title">{group}</div>{calendars.filter((calendar) => (calendar.group || "Calendars") === group).map((calendar) => <label className={`calendar-filter ${calendar.capability.read ? "" : "is-disabled"}`} key={calendar.id}><input type="checkbox" checked={selected.includes(calendar.id)} onChange={() => onToggle(calendar)} disabled={!calendar.capability.read} /><span className="checkmark" style={{ backgroundColor: calendar.color }} /><span className="calendar-name">{calendar.name}</span><span className="calendar-provider">{providerLabel(calendar.provider)}</span></label>)}</div>)}</div></aside>;
}

function providerLabel(provider: string) { return provider === "microsoft" ? "Microsoft 365" : provider === "google" ? "Google" : provider === "apple" ? "Apple" : provider; }

function CalendarEventContent({ arg }: { arg: EventContentArg }) {
  const event = arg.event.extendedProps.event as EventRecord | undefined;
  return <div className="fc-event-inner"><strong>{arg.timeText && <span className="fc-event-time-label">{arg.timeText}</span>}{arg.event.title}</strong>{event?.location && <span className="fc-event-location">{event.location}</span>}</div>;
}

function EventDrawer({ event, calendar, onClose, onEdit, onDelete }: { event: EventRecord; calendar?: CalendarRecord; onClose: () => void; onEdit: () => void; onDelete: () => void }) {
  const editable = !event.readOnly && Boolean(calendar?.capability.write);
  return <aside className="event-drawer" aria-label="Event details"><div className="drawer-header"><span>Event details</span><button className="icon-button" onClick={onClose} aria-label="Close event details"><X size={20} /></button></div><div className="drawer-content"><div className="drawer-title-row"><h2>{event.title || "Untitled event"}</h2></div><div className="event-source"><span className="source-swatch" style={{ backgroundColor: calendar?.color }} /><span>{calendar?.name ?? event.calendarId}</span><span className="source-provider">{providerLabel(calendar?.provider ?? event.source ?? "calendar")}</span>{event.readOnly && <span className="readonly-label">Read only</span>}</div><div className="drawer-divider" /><div className="detail-row"><Clock3 className="detail-icon" size={18} /><div><strong>{formatEventDate(event)}</strong><span>{formatEventTime(event)}</span>{event.recurrence?.isRecurring && <span>Repeats</span>}</div></div>{event.location && <div className="detail-row"><MapPin className="detail-icon" size={18} /><div><strong>Location</strong><span>{event.location}</span></div></div>}{event.description && <div className="detail-row"><AlignLeft className="detail-icon" size={18} /><div><strong>Description</strong><span className="description-text">{event.description}</span></div></div>}<div className="drawer-meta"><span>Calendar</span><strong>{calendar?.name ?? event.calendarId}</strong></div></div><div className="drawer-actions"><button className="button button-outline" disabled={!editable} onClick={onEdit}>Edit</button><button className="button button-danger" disabled={!editable || !calendar?.capability.delete} onClick={onDelete}>Delete</button></div></aside>;
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
