import { expect, test } from "@playwright/test";
import { calendarFixture } from "./fixtures/calendar";

for (const viewport of [{ name: "desktop", width: 1440, height: 1000 }, { name: "mobile", width: 390, height: 844 }]) {
  test(`resumes three parked calendars with saved credentials at ${viewport.name}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    const clock = new Date("2026-08-25T08:00:00Z");
    await page.clock.install({ time: clock });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const bootstrap = structuredClone(calendarFixture.bootstrap);
    const apple = bootstrap.calendars.find((calendar) => calendar.provider === "apple")!;
    bootstrap.calendars.push({ ...apple, id: "apple:personal", name: "Personal" }, { ...apple, id: "apple:shared", name: "Shared" });
    const ids = ["apple:family", "apple:personal", "apple:shared"];
    const refreshed: string[] = [];
    let phase: "parked" | "pending" | "ready" = "parked";
    const next = new Date(clock.getTime() + 15 * 60_000).toISOString();
    const last = new Date(clock.getTime() - 13 * 60 * 60_000).toISOString();
    await page.route("**/api/ui/**", async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.pathname.endsWith("/bootstrap")) return route.fulfill({ json: bootstrap });
      if (url.pathname.endsWith("/events")) return route.fulfill({ json: {
        items: calendarFixture.events, complete: phase === "ready",
        sources: bootstrap.calendars.map((calendar) => ({ provider: calendar.provider, calendar_id: calendar.id, complete: !ids.includes(calendar.id) || phase === "ready", status: ids.includes(calendar.id) ? phase : "ready", stale: ids.includes(calendar.id) && phase !== "ready", error_code: ids.includes(calendar.id) && phase === "parked" ? "auth" : null, next_sync_at: next, last_success_at: phase === "ready" ? new Date().toISOString() : last })),
      } });
      if (url.pathname.endsWith("/refresh")) {
        expect(request.method()).toBe("POST");
        expect(request.postData()).toBeNull();
        expect(request.headers()["x-csrf-token"]).toBe(bootstrap.csrf_token);
        refreshed.push(decodeURIComponent(url.pathname.split("/").at(-2)!));
        if (refreshed.length === 3) phase = "pending";
        return route.fulfill({ status: 202, json: { status: "accepted" } });
      }
      throw new Error(`Unexpected request: ${request.method()} ${url.pathname}`);
    });
    await page.goto("/spa/app");
    await expect(page.locator(".calendar-screen")).toBeVisible();
    await expect(page.locator(".fc-event").filter({ hasText: "Design review" }).first()).toBeVisible();
    await expect(page.locator(".event-status-row")).toContainText("3 calendars failed");
    await expect(page.locator(".event-status-row")).toContainText("saved credentials");
    await expect(page.locator(".event-status-row")).toContainText("Next automatic access check:");
    await expect(page.locator("input[type=password]")).toHaveCount(0);
    await expect(page.locator("vite-error-overlay")).toHaveCount(0);
    expect(await page.title()).not.toBe("");
    const resume = page.getByRole("button", { name: "Resume sync", exact: true });
    await expect(resume).toBeEnabled();
    const box = await resume.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(viewport.width);
    await page.screenshot({ path: `/tmp/calendar-auth-recovery-${viewport.name}.png` });
    await resume.focus();
    await page.keyboard.press("Enter");
    await expect(page.locator(".notice")).toContainText("3 calendar refreshes queued.");
    expect(refreshed.sort()).toEqual([...ids].sort());
    await expect(page.locator(".event-status-row")).toHaveText("Syncing");
    await expect(page.locator("input[type=password]")).toHaveCount(0);
    await expect(page).toHaveURL(/\/app$/);
    phase = "ready";
    await expect(page.locator(".event-status-row")).toContainText("Updated", { timeout: 10_000 });
    expect(errors).toEqual([]);
  });
}

test("a rejected recovery does not open a credential form or claim success", async ({ page }) => {
  const bootstrap = structuredClone(calendarFixture.bootstrap);
  await page.route("**/api/ui/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/bootstrap")) return route.fulfill({ json: bootstrap });
    if (url.pathname.endsWith("/events")) return route.fulfill({ json: { items: calendarFixture.events, complete: false, sources: [{ provider: "apple", calendar_id: "apple:family", complete: false, status: "parked", error_code: "auth", stale: true }] } });
    if (url.pathname.endsWith("/refresh")) return route.fulfill({ status: 409, json: { error: { code: "conflict", message: "Calendar sync is already running" } } });
    throw new Error(`Unexpected endpoint: ${url.pathname}`);
  });
  await page.goto("/spa/app");
  await page.getByRole("button", { name: "Resume sync", exact: true }).click();
  await expect(page.locator(".notice")).toContainText("1 refresh rejected.");
  await expect(page.locator(".event-status-row")).toContainText("1 calendar failed");
  await expect(page.locator("input[type=password]")).toHaveCount(0);
  await expect(page).toHaveURL(/\/app$/);
});
