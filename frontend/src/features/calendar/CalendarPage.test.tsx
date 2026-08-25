// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CalendarSidebar, ManageMenu, shouldShowDetailedEventContent, usesCompactCalendarLayout } from "./CalendarPage";
import type { CalendarRecord } from "../../lib/types";

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => { callback(0); return 0; });
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
});

function renderManageMenu() {
  act(() => root.render(<MemoryRouter initialEntries={["/app"]}><ManageMenu /></MemoryRouter>));
  const trigger = container.querySelector<HTMLButtonElement>("button[aria-haspopup='menu']");
  if (!trigger) throw new Error("Manage trigger was not rendered");
  return trigger;
}

function openMenu(trigger: HTMLButtonElement) {
  act(() => trigger.click());
  expect(trigger.getAttribute("aria-expanded")).toBe("true");
}

describe("calendar manage menu", () => {
  it("exposes the consolidated administration routes", () => {
    const trigger = renderManageMenu();
    openMenu(trigger);
    const links = Array.from(container.querySelectorAll<HTMLAnchorElement>("[role='menuitem']"));
    expect(links.map((link) => [link.textContent, link.getAttribute("href")])).toEqual([
      ["Connections", "/connections"],
      ["Sync activity", "/rules"],
      ["Settings", "/settings"],
    ]);
    expect(container.querySelector("a[href='/runs']")).toBeNull();
  });

  it("closes on Escape and restores focus to its trigger", () => {
    const trigger = renderManageMenu();
    openMenu(trigger);
    const outside = document.createElement("button");
    document.body.append(outside);
    outside.focus();
    act(() => document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })));
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(trigger);
    outside.remove();
  });

  it("closes on an outside pointer without stealing that focus", () => {
    const trigger = renderManageMenu();
    openMenu(trigger);
    const outside = document.createElement("button");
    document.body.append(outside);
    outside.focus();
    act(() => outside.dispatchEvent(new Event("pointerdown", { bubbles: true })));
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(outside);
    outside.remove();
  });
});

describe("calendar sidebar", () => {
  it("keeps its scope to event creation, date navigation, and visibility toggles", () => {
    const calendar: CalendarRecord = { id: "google:primary", name: "Personal", provider: "google", accountLabel: "Google Workspace", color: "#4762ee", capability: { read: true, create: true, write: true, delete: true, recurring: true } };
    const otherSource: CalendarRecord = { id: "microsoft:team", name: "Product", provider: "microsoft", accountLabel: "Microsoft 365", color: "#2d9b5c", capability: { read: true, create: true, write: true, delete: true, recurring: true } };
    const onToggle = vi.fn();
    const onRefresh = vi.fn();
    act(() => root.render(<CalendarSidebar calendars={[calendar, otherSource]} selected={[calendar.id, otherSource.id]} onToggle={onToggle} onCreate={() => undefined} onRefresh={onRefresh} refreshDisabled={false} refreshing={false} onClose={() => undefined} onJumpToDate={() => undefined} showClose={false} visibleDate={new Date(2026, 7, 25)} />));
    expect(container.textContent).toContain("New event");
    expect(container.textContent).toContain("Personal");
    expect(Array.from(container.querySelectorAll(".group-title"), (group) => group.textContent)).toEqual(["Google Workspace", "Microsoft 365"]);
    expect(container.querySelectorAll("a")).toHaveLength(0);
    expect(Array.from(container.querySelectorAll(".mini-weekdays span"), (day) => day.textContent)).toEqual(["M", "T", "W", "T", "F", "S", "S"]);
    const checkbox = container.querySelector<HTMLInputElement>("input[type='checkbox']");
    if (!checkbox) throw new Error("Calendar visibility checkbox was not rendered");
    act(() => checkbox.click());
    expect(onToggle).toHaveBeenCalledWith(calendar);
    const refresh = Array.from(container.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.includes("Refresh"));
    if (!refresh) throw new Error("Sidebar refresh button was not rendered");
    act(() => refresh.click());
    expect(onRefresh).toHaveBeenCalledOnce();
  });
});

describe("calendar responsive layout", () => {
  it("keeps drawers as overlays through mobile, iPad, and medium desktop widths", () => {
    expect(usesCompactCalendarLayout(390)).toBe(true);
    expect(usesCompactCalendarLayout(768)).toBe(true);
    expect(usesCompactCalendarLayout(1024)).toBe(true);
    expect(usesCompactCalendarLayout(1280)).toBe(true);
    expect(usesCompactCalendarLayout(1439)).toBe(true);
    expect(usesCompactCalendarLayout(1440)).toBe(false);
  });
});

describe("calendar event density", () => {
  it("uses available vertical space for events lasting an hour or longer", () => {
    expect(shouldShowDetailedEventContent(new Date("2026-08-26T09:00:00Z"), new Date("2026-08-26T10:00:00Z"), false)).toBe(true);
    expect(shouldShowDetailedEventContent(new Date("2026-08-26T09:00:00Z"), new Date("2026-08-26T09:30:00Z"), false)).toBe(false);
    expect(shouldShowDetailedEventContent(new Date("2026-08-26T09:00:00Z"), new Date("2026-08-27T09:00:00Z"), true)).toBe(false);
  });
});
