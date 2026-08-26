import type { Page } from "@playwright/test";
import { calendarMockFixture, createCalendarMockStore } from "../../src/dev/calendarMockData";
import type { CalendarMockState } from "../../src/dev/calendarMockData";

export const calendarFixture = calendarMockFixture;
export type CalendarFixtureOptions = { state?: CalendarMockState };

/** Intercepts every UI API request so the visual suite never reaches auth or production data. */
export async function installCalendarApiFixture(page: Page, options: CalendarFixtureOptions = {}) {
  const store = createCalendarMockStore(options.state ?? "populated");
  await page.unroute("**/api/ui/**");
  await page.route("**/api/ui/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const rawBody = request.postData();
    let body: Record<string, unknown> | undefined;
    if (rawBody) {
      try {
        const parsed: unknown = JSON.parse(rawBody);
        body = parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed as Record<string, unknown> : undefined;
      } catch { /* the shared mock treats invalid bodies as empty */ }
    }
    const result = store.handle({ method: request.method(), pathname: url.pathname, searchParams: url.searchParams, body });
    return route.fulfill(result.body === undefined ? { status: result.status } : { status: result.status, json: result.body });
  });
}
