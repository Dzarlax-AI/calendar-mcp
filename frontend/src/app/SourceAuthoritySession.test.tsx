// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { expect, it, vi } from "vitest";
import { BootstrapGate } from "./App";
import { useSourceAuthority, type SourceAuthority } from "./sourceAuthority";
import type { Bootstrap } from "../lib/types";

it("disposes old callbacks and clears event queries on a bootstrap session change", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } });
  const bootstrap = { username: "first", csrf_token: "first", calendars: [] } as unknown as Bootstrap;
  client.setQueryData(["bootstrap"], bootstrap);
  client.setQueryData(["events", "old"], { private: true });
  const seen: SourceAuthority[] = [];
  function Page() { seen.push(useSourceAuthority()); return <div>Workspace</div>; }
  const container = document.createElement("div");
  const root = createRoot(container);
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  await act(async () => root.render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/app"]}><Routes><Route element={<BootstrapGate />}><Route path="/app" element={<Page />} /></Route></Routes></MemoryRouter></QueryClientProvider>));
  const old = seen.at(-1)!;
  expect(old.isCurrent("a", 0)).toBe(true);
  await act(async () => { client.setQueryData(["bootstrap"], { ...bootstrap, username: "second", csrf_token: "second" }); await new Promise((resolve) => setTimeout(resolve, 10)); });
  expect(old.isCurrent("a", 0)).toBe(false);
  expect(seen.at(-1)).not.toBe(old);
  expect(client.getQueryData(["events", "old"])).toBeUndefined();
  act(() => root.unmount()); client.clear(); vi.unstubAllGlobals();
});
