// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CalendarRecord, RunRecord } from "../../lib/types";

const bootstrap = {
  csrf_token: "test-csrf",
  calendars: [] as CalendarRecord[],
  rules: [],
  runs: [] as RunRecord[],
  connections: [],
  settings: {},
};

vi.mock("../../app/App", () => ({ useBootstrapData: () => bootstrap }));

import ControlPlanePage from "./ControlPlanePage";

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
});

afterEach(() => {
  bootstrap.runs = [];
  bootstrap.calendars = [];
  bootstrap.settings = {};
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
});

describe("calendar selection", () => {
  it("shows a secure enable or disable form for each calendar", () => {
    const capability = { read: true, create: true, write: true, delete: true, recurring: false };
    bootstrap.calendars = [
      { id: "google:account:on", name: "Enabled", provider: "google", color: "blue", enabled: true, capability },
      { id: "google:account:off", name: "Disabled", provider: "google", color: "red", enabled: false, providerAccess: { read: true, write: false }, capability: { ...capability, read: false, write: false } },
      { id: "google:account:noaccess", name: "No provider access", provider: "google", color: "red", enabled: false, providerAccess: { read: false, write: false }, capability: { ...capability, read: false, write: false } },
    ];
    act(() => root.render(<MemoryRouter><ControlPlanePage section="connections" /></MemoryRouter>));
    const forms = Array.from(container.querySelectorAll<HTMLFormElement>('form[action*="/calendars/"]'));
    expect(forms).toHaveLength(3);
    expect(forms[0].querySelector('[name="enabled"]')?.getAttribute("value")).toBe("false");
    expect(forms[1].querySelector('[name="enabled"]')?.getAttribute("value")).toBe("true");
    expect(forms[0].querySelector('[name="csrf_token"]')?.getAttribute("value")).toBe("test-csrf");
    expect(forms[1].textContent).toContain("Enable");
    expect(container.textContent).toContain("Disabled · Last reported: read only");
    expect(container.textContent).toContain("Disabled · Last reported: no access");
  });
});

describe("calendar display setting", () => {
  it("offers a service-wide CSRF-protected setting in both states", () => {
    bootstrap.settings = { hideDeclinedEvents: false };
    act(() => root.render(<MemoryRouter><ControlPlanePage section="settings" /></MemoryRouter>));
    let form = container.querySelector<HTMLFormElement>('form[action="/settings/calendar-display"]');
    expect(form?.textContent).toContain("Hide declined events");
    expect(form?.querySelector('[name="csrf_token"]')?.getAttribute("value")).toBe("test-csrf");
    expect(form?.querySelector('[name="hide_declined_events"]')?.getAttribute("value")).toBe("true");
    bootstrap.settings = { hideDeclinedEvents: true };
    act(() => root.render(<MemoryRouter><ControlPlanePage section="settings" /></MemoryRouter>));
    form = container.querySelector<HTMLFormElement>('form[action="/settings/calendar-display"]');
    expect(form?.textContent).toContain("Show declined events");
    expect(form?.querySelector('[name="hide_declined_events"]')?.getAttribute("value")).toBe("false");
  });
});

describe("sync activity", () => {
  it("shows the server explanation for a successful standalone fallback", () => {
    bootstrap.runs = [{ id: "run", status: "succeeded", outcome: "succeeded", message: "Some recurring events are mirrored as individual events because their source series was unavailable." }];
    act(() => root.render(<MemoryRouter><ControlPlanePage section="rules" /></MemoryRouter>));
    expect(container.textContent).toContain("mirrored as individual events");
    expect(container.querySelector(".status-pill.success")?.textContent).toBe("succeeded");
  });

  it("keeps rules and recent activity on one page", () => {
    act(() => root.render(<MemoryRouter><ControlPlanePage section="rules" /></MemoryRouter>));

    expect(container.querySelector("h1")?.textContent).toBe("Sync activity");
    expect(Array.from(container.querySelectorAll("h2")).map((heading) => heading.textContent)).toEqual(expect.arrayContaining(["Sync rules", "Recent activity"]));
    expect(container.textContent).toContain("No sync rules");
    expect(container.textContent).toContain("No activity yet");
    expect(container.querySelector("a[href='/rules/new']")?.textContent).toContain("New rule");
  });
});
