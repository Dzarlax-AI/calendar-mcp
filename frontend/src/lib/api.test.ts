import { describe, expect, it, vi } from "vitest";
import { createEvent, deleteEvent, getBootstrap, getEvents, navigateToApp, refreshCalendar, refreshCalendars } from "./api";

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
    const created = await createEvent("csrf-token", { calendar_id: "c1", title: "Test", start: { date: "2026-09-15" }, end: { date: "2026-09-16" } });
    expect(created.warnings).toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith("/api/ui/events", expect.objectContaining({ credentials: "same-origin", method: "POST" }));
    expect((fetchMock.mock.calls[0][1].headers as Headers).get("X-CSRF-Token")).toBe("csrf-token");
    const body = JSON.parse(fetchMock.mock.calls[0][1].body);
    expect(body).not.toHaveProperty("attendees");
    expect(body).not.toHaveProperty("notification_policy");
    vi.unstubAllGlobals();
  });

  it("preserves bounded reconciliation warnings from successful mutations", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: "e1", calendar_id: "c1", title: "Test", start: { date: "2026-09-15" }, end: { date: "2026-09-16" }, warnings: ["Calendar data will refresh shortly."] }), { status: 201 }));
    vi.stubGlobal("fetch", fetchMock);
    const created = await createEvent("csrf-token", { calendar_id: "c1", title: "Test", start: { date: "2026-09-15" }, end: { date: "2026-09-16" } });
    expect(created.warnings).toEqual(["Calendar data will refresh shortly."]);
    vi.unstubAllGlobals();
  });

  it("bounds and normalizes delete reconciliation warnings", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ warnings: ["one", "two", "three", "four", 5, "x".repeat(201)] }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await deleteEvent("csrf-token", "c1", "e1");
    expect(result.warnings).toEqual(["one", "two", "three"]);
    vi.unstubAllGlobals();
  });

  it("normalizes cached source freshness without exposing raw errors", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ items: [], complete: false, sources: [{ provider: "google", calendar_id: "c1", complete: false, status: "failed", stale: true, last_success_at: null, next_sync_at: "2026-09-15T12:01:00Z", error_code: "provider_unavailable", error: { message: "secret provider payload" } }] }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await getEvents("2026-09-15T00:00:00Z", "2026-09-16T00:00:00Z", ["c1"]);
    expect(result.sources?.[0]).toMatchObject({ calendar_id: "c1", status: "failed", stale: true, next_sync_at: "2026-09-15T12:01:00Z", error_code: "provider_unavailable", error: "Calendar provider is temporarily unavailable" });
    expect(result.sources?.[0]?.error).not.toContain("secret");
    vi.unstubAllGlobals();
  });

  it("preserves the provider description format for event rendering", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ items: [{ id: "e1", calendar_id: "microsoft:c1", provider: "microsoft", title: "Review", description: "<p><strong>Agenda</strong></p>", description_format: "html", html_link: "https://outlook.office.com/calendar/event", conference: { solution: "teamsForBusiness", entry_points: [{ type: "video", uri: "https://teams.microsoft.com/l/meetup-join/example" }] }, start: { date: "2026-09-15" }, end: { date: "2026-09-16" } }], complete: true }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await getEvents("2026-09-15T00:00:00Z", "2026-09-16T00:00:00Z", ["microsoft:c1"]);
    expect(result.items[0]).toMatchObject({ descriptionFormat: "html", description: "<p><strong>Agenda</strong></p>", htmlLink: "https://outlook.office.com/calendar/event", conference: { entry_points: [{ type: "video", uri: "https://teams.microsoft.com/l/meetup-join/example" }] } });
    vi.unstubAllGlobals();
  });

  it("treats cached Google descriptions without format metadata as HTML", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ items: [{ id: "e1", calendar_id: "google:c1", provider: "google", description: "<p><strong>Agenda</strong></p>", start: { date: "2026-09-15" }, end: { date: "2026-09-16" } }], complete: true }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await getEvents("2026-09-15T00:00:00Z", "2026-09-16T00:00:00Z", ["google:c1"]);
    expect(result.items[0]).toMatchObject({ source: "google", descriptionFormat: "html" });
    vi.unstubAllGlobals();
  });

  it("preserves degraded status while hiding provider payloads", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ items: [], complete: false, sources: [{ provider: "apple", calendar_id: "c1", complete: false, status: "degraded", stale: true, error_code: "protocol", error: { message: "raw iCalendar payload" } }] }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await getEvents("2026-09-15T00:00:00Z", "2026-09-16T00:00:00Z", ["c1"]);
    expect(result.sources?.[0]).toMatchObject({ calendar_id: "c1", status: "degraded", error_code: "protocol" });
    expect(result.sources?.[0]?.error).not.toContain("iCalendar");
    vi.unstubAllGlobals();
  });

  it("enqueues a calendar refresh with CSRF protection", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
    vi.stubGlobal("fetch", fetchMock);
    await refreshCalendar("csrf-token", "google:primary/calendar");
    expect(fetchMock).toHaveBeenCalledWith("/api/ui/calendars/google%3Aprimary%2Fcalendar/refresh", expect.objectContaining({ method: "POST", credentials: "same-origin" }));
    expect((fetchMock.mock.calls[0][1].headers as Headers).get("X-CSRF-Token")).toBe("csrf-token");
    vi.unstubAllGlobals();
  });

  it("classifies an Authentik redirect as session loss without following it", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 0, type: "opaqueredirect", text: async () => "" });
    vi.stubGlobal("fetch", fetchMock);
    await expect(refreshCalendar("csrf-token", "c1")).rejects.toMatchObject({ code: "session_expired", status: 401 });
    expect(fetchMock).toHaveBeenCalledWith("/api/ui/calendars/c1/refresh", expect.objectContaining({ redirect: "manual", method: "POST" }));
    vi.unstubAllGlobals();
  });

  it("aggregates queued, session-expired, and failed refresh outcomes", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(null, { status: 202 }))
      .mockResolvedValueOnce({ ok: false, status: 0, type: "opaqueredirect", text: async () => "" })
      .mockResolvedValueOnce(new Response(JSON.stringify({ code: "conflict", message: "already running" }), { status: 409 }))
      .mockRejectedValueOnce(new Error("offline"));
    vi.stubGlobal("fetch", fetchMock);
    await expect(refreshCalendars("csrf-token", ["queued", "expired", "rejected", "unknown"])).resolves.toEqual({ queued: ["queued"], sessionExpired: ["expired"], rejected: ["rejected"], unknown: ["unknown"] });
    vi.unstubAllGlobals();
  });

  it("keeps top-level navigation testable", () => {
    const navigate = vi.fn();
    navigateToApp(navigate);
    expect(navigate).toHaveBeenCalledWith("/app");
  });
});
