// @vitest-environment jsdom
import { act, forwardRef, useEffect, useImperativeHandle } from "react";
import { createRoot } from "react-dom/client";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { EventRecord } from "../../lib/types";

const mock = vi.hoisted(() => ({ getEvents: vi.fn(), createEvent: vi.fn(), updateEvent: vi.fn(), deleteEvent: vi.fn(), revert: vi.fn() }));
vi.mock("../../lib/api", async (importOriginal) => ({ ...await importOriginal<typeof import("../../lib/api")>(), ...mock }));
vi.mock("../../app/App", () => ({ useBootstrapData: () => ({ csrf_token: "csrf", settings: {}, calendars: ["a", "b"].map((id) => ({ id, name: id, provider: "google", color: "blue", capability: { read: true, create: true, write: true, delete: true, recurring: true } })) }) }));
vi.mock("@fullcalendar/react", () => ({ default: forwardRef(function MockCalendar(props: any, ref) {
  useImperativeHandle(ref, () => ({ getApi: () => ({ view: { title: "Calendar" }, changeView: () => {}, getDate: () => new Date("2026-09-15"), prev: () => props.datesSet({ start: new Date("2026-09-14"), end: new Date("2026-09-21"), view: { calendar: { getDate: () => new Date("2026-09-15") } } }), next: () => props.datesSet({ start: new Date("2026-09-21"), end: new Date("2026-09-28"), view: { calendar: { getDate: () => new Date("2026-09-22") } } }), today: () => {} }) }));
  useEffect(() => { props.datesSet({ start: new Date("2026-09-14"), end: new Date("2026-09-21"), view: { calendar: { getDate: () => new Date("2026-09-15") } } }); }, []);
  return <div>{props.events.map((event: any) => <button key={event.id} onClick={() => props.eventClick({ event: { id: event.id, extendedProps: event.extendedProps } })}>{event.title}</button>)}{props.events.map((event: any) => <button key={`move-${event.id}`} onClick={() => props.eventDrop({ event: { id: event.id, extendedProps: event.extendedProps, start: new Date("2026-09-15T12:00:00Z"), end: new Date("2026-09-15T13:00:00Z") }, revert: mock.revert })}>Move {event.title}</button>)}</div>;
}) }));
import CalendarPage from "./CalendarPage";
import { SourceAuthority, SourceAuthorityProvider } from "../../app/sourceAuthority";

const event = (calendarId: string, recurring = false): EventRecord => ({ id: calendarId, calendarId, title: `Event ${calendarId}`, start: "2026-09-15T10:00:00Z", end: "2026-09-15T11:00:00Z", allDay: false, recurrence: { isRecurring: recurring } });
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;
let client: QueryClient;
let items: EventRecord[];
let authority: SourceAuthority;
const renderPage = (show: boolean) => <QueryClientProvider client={client}><MemoryRouter><SourceAuthorityProvider authority={authority}>{show && <CalendarPage />}</SourceAuthorityProvider></MemoryRouter></QueryClientProvider>;
const response = (status = "ready") => {
  const complete = status === "ready" || status === "disabled";
  return { items, complete, sources: [{ calendar_id: "a", provider: "google", status, complete }, { calendar_id: "b", provider: "google", status: "ready", complete: true }] };
};
const flush = async () => { await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); }); };
function button(text: string) { const found = [...container.querySelectorAll("button")].find((node) => node.textContent?.trim() === text); if (!found) throw Error(`Missing button ${text}`); return found; }
function click(text: string) { act(() => button(text).click()); }
function openNewEvent() {
  click("New event");
  const input = container.querySelector<HTMLInputElement>('input[placeholder="Add a title"]')!;
  act(() => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, "New calendar entry");
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}
async function push(status: string) { mock.getEvents.mockImplementation(async () => response(status)); await act(async () => { await client.refetchQueries({ queryKey: ["events"], type: "active" }); }); await flush(); }
beforeEach(async () => {
  localStorage.clear(); items = [event("a"), event("b")]; mock.getEvents.mockImplementation(async () => response()); mock.createEvent.mockReset(); mock.updateEvent.mockReset(); mock.deleteEvent.mockReset(); mock.revert.mockReset();
  container = document.createElement("div"); document.body.append(container); root = createRoot(container); client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 15_000 } } });
  authority = new SourceAuthority();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true); vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => { callback(0); return 0; });
  await act(async () => root.render(renderPage(true))); await flush();
});
afterEach(() => { onlineManager.setOnline(true); act(() => root.unmount()); client.clear(); container.remove(); vi.unstubAllGlobals(); });

describe("source access revocation", () => {
  it.each(["details", "edit", "delete", "scope"])("closes %s for the revoked source", async (surface) => {
    if (surface === "scope") { items = [event("a", true), event("b")]; await push("ready"); }
    click("Event a");
    if (surface === "edit") click("Edit");
    if (surface === "delete" || surface === "scope") click("Delete");
    expect(container.textContent).toContain(surface === "details" ? "Event details" : surface === "edit" ? "Edit event" : surface === "delete" ? "Delete event?" : "Choose what to delete");
    await push("unavailable");
    expect(container.textContent).not.toContain("Event a");
    expect(container.textContent).not.toContain("Event details");
    expect(container.textContent).not.toContain("Edit event");
    expect(container.textContent).not.toContain("Delete event?");
    expect(container.textContent).not.toContain("Choose what to delete");
    expect(container.textContent).toContain("Event b");
    expect(mock.deleteEvent).not.toHaveBeenCalled(); expect(mock.updateEvent).not.toHaveBeenCalled();
  });
  it("keeps another source open", async () => {
    click("Event b"); await push("unavailable"); expect(container.textContent).toContain("Event details");
    expect(container.textContent).not.toContain("Event a");
  });
  it("does not close a form for temporary failure or pending status", async () => {
    click("Event a"); click("Edit");
    await push("failed"); expect(container.textContent).toContain("Edit event");
    await push("pending"); expect(container.textContent).toContain("Edit event");
  });
  it("ignores a successful update that finishes after revocation", async () => {
    let resolve!: (event: EventRecord) => void;
    mock.updateEvent.mockImplementation(() => new Promise((done) => { resolve = done; }));
    click("Event a"); click("Edit"); click("Save changes"); await flush();
    expect(mock.updateEvent).toHaveBeenCalledOnce();
    await push("unavailable");
    await act(async () => resolve(event("a")));
    expect(container.textContent).not.toContain("Event a");
    expect(container.textContent).not.toContain("Event details");
    expect(container.textContent).not.toContain("Event updated");
    expect(mock.updateEvent).toHaveBeenCalledOnce();
  });

  it("does not revert a dragged recurring event after revocation", async () => {
    items = [event("a", true), event("b")]; await push("ready");
    click("Move Event a"); expect(container.textContent).toContain("Choose what to change");
    await push("unavailable");
    expect(container.textContent).not.toContain("Choose what to change");
    expect(mock.revert).not.toHaveBeenCalled();
    expect(mock.updateEvent).not.toHaveBeenCalled();
  });
  it("retains an open form while a new range is loading placeholder data", async () => {
    click("Event a"); click("Edit");
    mock.getEvents.mockImplementation(() => new Promise(() => {}));
    const next = container.querySelector<HTMLButtonElement>('button[aria-label="Next period"]');
    if (!next) throw Error("Missing navigation");
    act(() => next.click()); await flush();
    expect(container.textContent).toContain("Edit event");
  });

  it("keeps revocation across cached ranges until a newer fetch restores authority", async () => {
    const nav = (label: string) => { const control = container.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`); if (!control) throw Error("Missing navigation"); act(() => control.click()); };
    nav("Next period"); await flush();
    await push("unavailable"); expect(container.textContent).not.toContain("Event a");
    nav("Previous period"); await flush();
    expect(container.textContent).not.toContain("Event a");
    expect(container.textContent).toContain("Event b");
    await push("ready");
    click("Event a"); expect(container.textContent).toContain("Event details");
    nav("Next period"); await flush();
    expect(container.textContent).toContain("Event details");
    expect([...container.querySelectorAll("button")].some((node) => node.textContent?.trim() === "Event a")).toBe(false);
  });

  it("retains revocation after leaving and returning to a cached range", async () => {
    const nav = (label: string) => { const control = container.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`); if (!control) throw Error("Missing navigation"); act(() => control.click()); };
    nav("Next period"); await flush();
    await push("unavailable");
    await act(async () => root.render(renderPage(false)));
    mock.getEvents.mockImplementation(() => new Promise(() => {}));
    await act(async () => root.render(renderPage(true))); await flush();
    expect(container.textContent).not.toContain("Event a");
    expect(container.textContent).toContain("Event b");
    await push("ready");
    expect(container.textContent).toContain("Event a");
    nav("Next period"); await flush();
    expect(container.textContent).not.toContain("Event a");
  });

  it("applies a late revocation response after returning to a different cached range", async () => {
    let finish!: (value: ReturnType<typeof response>) => void;
    mock.getEvents.mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    const next = container.querySelector<HTMLButtonElement>('button[aria-label="Next period"]');
    if (!next) throw Error("Missing navigation");
    act(() => next.click()); await flush();
    await act(async () => root.render(renderPage(false)));
    await act(async () => root.render(renderPage(true))); await flush();
    click("Event a");
    await act(async () => finish(response("unavailable"))); await flush();
    expect(container.textContent).not.toContain("Event a");
    expect(container.textContent).not.toContain("Event details");
    expect(container.textContent).toContain("Event b");
  });

  it.each(["unavailable", "disabled"])("applies late %s and only recovers from a request started afterward", async (blockedStatus) => {
    function navigatePeriod(direction: "Next" | "Previous") {
      const control = container.querySelector<HTMLButtonElement>(`button[aria-label="${direction} period"]`);
      if (!control) throw Error("Missing period navigation");
      act(() => control.click());
    }

    let finishRevoked!: (value: ReturnType<typeof response>) => void;
    mock.getEvents.mockImplementationOnce(() => new Promise((resolve) => { finishRevoked = resolve; }));
    navigatePeriod("Next");
    await flush();
    navigatePeriod("Previous");
    await flush();
    await push("ready");
    click("Event a");
    click("Edit");
    expect(container.textContent).toContain("Edit event");

    let finishEarlierReady!: (value: ReturnType<typeof response>) => void;
    mock.getEvents.mockImplementationOnce(() => new Promise((resolve) => { finishEarlierReady = resolve; }));
    act(() => { void client.refetchQueries({ queryKey: ["events"], type: "active" }); });
    await flush();

    await act(async () => finishRevoked(response(blockedStatus)));
    await flush();
    expect(container.textContent).not.toContain("Event a");
    expect(container.textContent).not.toContain("Edit event");
    expect(container.textContent).toContain("Event b");
    expect(container.textContent?.includes("1 calendar failed")).toBe(blockedStatus === "unavailable");

    await act(async () => finishEarlierReady(response("ready")));
    await flush();
    expect(container.textContent).not.toContain("Event a");
    expect(container.textContent?.includes("1 calendar failed")).toBe(blockedStatus === "unavailable");

    await push("ready");
    expect(container.textContent).toContain("Event a");
    expect(container.textContent).not.toContain("Edit event");
    expect(container.textContent).not.toContain("1 calendar failed");
    expect(mock.updateEvent).not.toHaveBeenCalled();
  });

  it.each([
    ["parked", "Resume sync"],
    ["degraded", "Refresh affected calendars"],
  ])("replaces the %s status and its action after revocation", async (status, action) => {
    await push(status);
    expect(button(action)).toBeDefined();
    act(() => authority.observe(authority.nextOrder(), response("unavailable").sources));
    expect(container.textContent).toContain("1 calendar failed");
    expect(container.textContent).not.toContain(action);
    expect(container.textContent).toContain("Event b");
  });

  it("omits recovery actions for a deselected source while keeping range placeholders", async () => {
    await push("parked");
    expect(button("Resume sync")).toBeDefined();
    mock.getEvents.mockImplementation(() => new Promise(() => {}));
    const firstCalendar = container.querySelector<HTMLInputElement>('.calendar-filter input')!;
    act(() => firstCalendar.click());
    await flush();
    act(() => authority.observe(authority.nextOrder(), response("unavailable").sources));
    expect(container.textContent).not.toContain("Resume sync");
    expect(container.textContent).not.toContain("1 calendar failed");
    expect(container.textContent).toContain("Event b");
  });

  it("refreshes the range without scheduling sync when the only selected source is blocked", async () => {
    const secondCalendar = container.querySelectorAll<HTMLInputElement>('.calendar-filter input')[1]!;
    act(() => secondCalendar.click());
    await flush();
    await push("unavailable");
    expect(container.textContent).not.toContain("Event a");
    mock.getEvents.mockImplementation(async () => response("ready"));
    const refresh = container.querySelector<HTMLButtonElement>('button[aria-label="Refresh selected calendars"]')!;
    act(() => refresh.click());
    await flush();
    expect(container.textContent).toContain("Event a");
    expect(client.getMutationCache().getAll()).toHaveLength(0);
  });

  it("preserves the draft without redirecting a revoked create target", async () => {
    openNewEvent();
    expect(button("Create event").disabled).toBe(false);
    act(() => authority.observe(authority.nextOrder(), response("unavailable").sources));
    const select = container.querySelector<HTMLSelectElement>('form select')!;
    expect(Array.from(select.options, ({ value }) => value)).toEqual(["", "b"]);
    expect(select.value).toBe("");
    expect(button("Create event").disabled).toBe(true);
    expect(container.querySelector<HTMLInputElement>('input[placeholder="Add a title"]')?.value).toBe("New calendar entry");

    act(() => { select.value = "b"; select.dispatchEvent(new Event("change", { bubbles: true })); });
    mock.createEvent.mockResolvedValueOnce(event("b"));
    click("Create event");
    await flush();
    expect(mock.createEvent).toHaveBeenCalledExactlyOnceWith("csrf", expect.objectContaining({ calendar_id: "b", title: "New calendar entry" }));
  });

  it("blocks a paused create after revoke and allows a new submission after ready", async () => {
    openNewEvent();
    act(() => onlineManager.setOnline(false));
    click("Create event");
    await flush();
    expect(client.getMutationCache().getAll()[0]?.state.isPaused).toBe(true);
    act(() => {
      authority.observe(authority.nextOrder(), response("unavailable").sources);
      authority.observe(authority.nextOrder(), response("ready").sources);
    });
    await act(async () => { onlineManager.setOnline(true); await client.resumePausedMutations(); });
    await flush();
    expect(mock.createEvent).not.toHaveBeenCalled();
    mock.createEvent.mockResolvedValueOnce(event("a"));
    click("Create event");
    await flush();
    expect(mock.createEvent).toHaveBeenCalledExactlyOnceWith("csrf", expect.objectContaining({ calendar_id: "a" }));
  });

  it("ignores a create completion from a revoked revision", async () => {
    let finish!: (value: EventRecord) => void;
    mock.createEvent.mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    openNewEvent();
    click("Create event");
    await flush();
    act(() => authority.observe(authority.nextOrder(), response("unavailable").sources));
    await act(async () => finish(event("a")));
    await flush();
    expect(container.textContent).not.toContain("Event created");
    expect(container.querySelector('input[placeholder="Add a title"]')).not.toBeNull();
  });

  it.each(["details", "edit", "delete", "scope"])("discards an older %s after batched revoke and ready responses", async (surface) => {
    if (surface === "scope") { items = [event("a", true), event("b")]; await push("ready"); }
    click("Event a");
    if (surface === "edit") click("Edit");
    if (surface === "delete" || surface === "scope") click("Delete");
    const title = surface === "details" ? "Event details" : surface === "edit" ? "Edit event" : surface === "delete" ? "Delete event?" : "Choose what to delete";
    expect(container.textContent).toContain(title);
    act(() => {
      authority.observe(authority.nextOrder(), response("unavailable").sources);
      authority.observe(authority.nextOrder(), response("ready").sources);
    });
    expect(container.textContent).not.toContain(title);
    expect(container.textContent).toContain("Event b");
    expect(mock.updateEvent).not.toHaveBeenCalled();
    expect(mock.deleteEvent).not.toHaveBeenCalled();
  });

  it("blocks a paused mutation from a previous mount after revocation", async () => {
    click("Event a"); click("Edit");
    onlineManager.setOnline(false); click("Save changes"); await flush();
    await act(async () => root.render(renderPage(false)));
    await act(async () => root.render(renderPage(true))); await flush();
    act(() => authority.observe(authority.nextOrder(), response("unavailable").sources));
    await flush();
    await act(async () => { onlineManager.setOnline(true); await client.resumePausedMutations(); });
    expect(mock.updateEvent).not.toHaveBeenCalled();
  });

  it("does not dispatch a paused update after revocation", async () => {
    click("Event a"); click("Edit");
    onlineManager.setOnline(false);
    click("Save changes"); await flush();
    expect(mock.updateEvent).not.toHaveBeenCalled();
    act(() => authority.observe(authority.nextOrder(), response("unavailable").sources));
    await flush();
    await act(async () => { onlineManager.setOnline(true); await client.resumePausedMutations(); });
    expect(mock.updateEvent).not.toHaveBeenCalled();
  });

});
