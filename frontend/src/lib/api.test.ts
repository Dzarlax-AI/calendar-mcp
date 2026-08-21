import { describe, expect, it, vi } from "vitest";
import { createEvent, getBootstrap } from "./api";

describe("browser API client", () => {
  it("normalizes the flat bootstrap contract", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ csrf_token: "csrf", calendars: [{ id: "c1", name: "Personal", provider: "google", can_read: true, can_write: false, supports_recurrence: false }], connections: [], rules: [], runs: [], settings: { mcp_endpoint: "https://example.test/mcp", legacy_api_key_configured: false }, capabilities: {} }), { status: 200, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    const bootstrap = await getBootstrap();
    expect(bootstrap.calendars[0]).toMatchObject({ id: "c1", capability: { read: true, write: false }, readOnly: true });
    expect(bootstrap.settings.mcpEndpoint).toBe("https://example.test/mcp");
    vi.unstubAllGlobals();
  });

  it("sends same-origin credentials and the CSRF header on creates", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: "e1", calendar_id: "c1", title: "Test", start: { date: "2026-09-15" }, end: { date: "2026-09-16" } }), { status: 201, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    await createEvent("csrf-token", { calendar_id: "c1", title: "Test", start: { date: "2026-09-15" }, end: { date: "2026-09-16" } });
    expect(fetchMock).toHaveBeenCalledWith("/api/ui/events", expect.objectContaining({ credentials: "same-origin", method: "POST" }));
    expect((fetchMock.mock.calls[0][1].headers as Headers).get("X-CSRF-Token")).toBe("csrf-token");
    const body = JSON.parse(fetchMock.mock.calls[0][1].body);
    expect(body).not.toHaveProperty("attendees");
    expect(body).not.toHaveProperty("notification_policy");
    vi.unstubAllGlobals();
  });
});
