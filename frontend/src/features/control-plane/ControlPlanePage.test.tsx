// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { RunRecord } from "../../lib/types";

const bootstrap = {
  csrf_token: "test-csrf",
  calendars: [],
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
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
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
