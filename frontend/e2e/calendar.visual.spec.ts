import { expect, test, type Page } from "@playwright/test";
import { installCalendarApiFixture } from "./fixtures/calendar";

const primaryViewports = [
  { name: "mobile", width: 390, height: 844 },
  { name: "tablet", width: 768, height: 1024 },
  { name: "small-desktop", width: 1024, height: 1366 },
  { name: "laptop", width: 1280, height: 800 },
  { name: "desktop", width: 1440, height: 900 },
] as const;

const frozenPages = new WeakSet<Page>();

async function openCalendar(page: Page, state: Parameters<typeof installCalendarApiFixture>[1] = {}) {
  if (!frozenPages.has(page)) {
    await page.clock.install({ time: new Date("2026-08-25T08:00:00Z") });
    frozenPages.add(page);
  }
  await installCalendarApiFixture(page, state);
  await page.addInitScript(() => localStorage.clear());
  await page.goto("/spa/app");
  await expect(page.getByRole("button", { name: "New event" }).first()).toBeVisible();
  await expect(page.locator(".fc")).toBeVisible();
}

async function expectNoHorizontalOverflow(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

for (const viewport of primaryViewports) {
  test(`populated week remains reviewable at ${viewport.name}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await openCalendar(page);
    await expectNoHorizontalOverflow(page);
    if (viewport.width < 1440) await expect(page.getByRole("button", { name: "Choose calendars" })).toBeVisible();
    else await expect(page.getByRole("button", { name: "Manage" })).toBeVisible();
    await expect(page.locator(".calendar-screen")).toHaveScreenshot(`calendar-populated-${viewport.name}.png`, { animations: "disabled" });
  });

  test(`rich event details remain usable at ${viewport.name}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await openCalendar(page);
    await page.getByText("Quarterly planning with a deliberately long title", { exact: false }).first().click();
    const eventSurface = viewport.width < 1440
      ? page.getByRole("dialog", { name: "Event details" })
      : page.getByRole("complementary", { name: "Event details" });
    await expect(eventSurface).toBeVisible();
    await expect(page.getByRole("button", { name: "Close event details" })).toBeVisible();
    await expectNoHorizontalOverflow(page);
    await expect(page.locator(".calendar-screen")).toHaveScreenshot(`calendar-event-details-${viewport.name}.png`, { animations: "disabled" });
    await page.getByRole("button", { name: "Close event details" }).click();
    await expect(eventSurface).toBeHidden();
  });

  test(`create modal and filter surface fit at ${viewport.name}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await openCalendar(page);
    await page.getByRole("button", { name: "New event" }).first().click();
    const dialog = page.getByRole("dialog", { name: "New event" });
    await expect(dialog).toBeVisible();
    await dialog.getByText("All day").click();
    await expect(dialog.getByRole("button", { name: "Create event" })).toBeVisible();
    await expectNoHorizontalOverflow(page);
    await expect(dialog).toHaveScreenshot(`calendar-create-${viewport.name}.png`, { animations: "disabled" });
    await page.getByRole("button", { name: "Close dialog" }).click();
    if (viewport.width < 1440) {
      await page.getByRole("button", { name: /Choose calendars|Calendars/ }).first().click();
    }
    await expect(page.getByText("Imported calendar with a very long disabled label")).toBeVisible();
    await expect(page.getByLabel(/Imported calendar.*calendar/)).toBeDisabled();
    await expectNoHorizontalOverflow(page);
    if (viewport.width < 1440) {
      await expect(page.locator(".calendar-screen")).toHaveScreenshot(`calendar-filters-${viewport.name}.png`, { animations: "disabled" });
    }
  });
}

test("toolbar actions, sidebar Manage menu, and every view selector stay reachable", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openCalendar(page);
  for (const name of ["Day", "Week", "Month", "List"]) {
    const viewButton = page.getByRole("button", { name, exact: true });
    await viewButton.click();
    await expect(viewButton).toHaveAttribute("aria-pressed", "true");
    if (name === "Week") {
      await expect(page.locator(".calendar-canvas-wrap")).toHaveClass(/is-phone-week/);
      await expect(page.locator(".fc-col-header-cell")).toHaveCount(3);
    }
    if (name === "Month") {
      await expect(page.locator(".mobile-month")).toBeVisible();
      await expect(page.getByRole("button", { name: "Today", exact: true })).toHaveCount(1);
      await expect(page.getByRole("button", { name: "New event", exact: true })).toHaveCount(1);
      await expect(page.getByRole("button", { name: "Choose calendars", exact: true })).toHaveCount(1);
      await expect(page.getByRole("heading", { name: "August", exact: true })).toHaveCount(1);
      const nextMonth = page.getByRole("button", { name: "Next month", exact: true });
      await expect(nextMonth).toHaveCount(1);
      await nextMonth.click();
      await expect(page.getByRole("heading", { name: "September", exact: true })).toBeVisible();
    }
  }
  await page.getByRole("button", { name: "Choose calendars" }).click();
  const sidebar = page.getByRole("dialog", { name: "Calendar" });
  await expect(sidebar.getByRole("button", { name: "Refresh selected calendars" })).toBeVisible();
  await sidebar.getByRole("button", { name: "Manage" }).click();
  const manageMenu = page.getByRole("menu", { name: "Manage calendar" });
  await expect(manageMenu).toBeVisible();
  await expect(manageMenu.getByRole("menuitem", { name: "Sync activity" })).toHaveAttribute("href", "/rules");
  await expect(manageMenu.getByRole("menuitem", { name: "Runs" })).toHaveCount(0);
  await expectNoHorizontalOverflow(page);
});

test("toolbar keeps phone actions compact at 731px", async ({ page }) => {
  await page.setViewportSize({ width: 731, height: 900 });
  await openCalendar(page);
  const create = page.getByRole("button", { name: "New event" });
  const filters = page.getByRole("button", { name: "Choose calendars" });
  await expect(create).toBeVisible();
  await expect(filters).toBeVisible();
  const createBox = await create.boundingBox();
  const filtersBox = await filters.boundingBox();
  expect(createBox?.width).toBeLessThanOrEqual(44);
  expect(filtersBox?.width).toBeLessThanOrEqual(44);
  expect(Math.abs((createBox?.y ?? 0) - (filtersBox?.y ?? 0))).toBeLessThanOrEqual(4);
  await expectNoHorizontalOverflow(page);
});

for (const width of [560, 561, 820, 821, 1080, 1081, 1439, 1440]) {
  test(`responsive mode changes cleanly at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await openCalendar(page);
    await expectNoHorizontalOverflow(page);
    if (width < 1440) {
      await page.getByRole("button", { name: "Choose calendars" }).click();
      const dialog = page.getByRole("dialog", { name: "Calendar" });
      await expect(dialog).toBeVisible();
      await expect(dialog).toHaveCSS("position", "fixed");
    } else {
      await expect(page.getByRole("complementary", { name: "Calendar filters" })).toBeVisible();
    }
  });
}

test("degraded, empty, error, recurring delete, and no-writable states are explicit", async ({ page }) => {
  await page.setViewportSize({ width: 768, height: 1024 });
  await openCalendar(page, { state: "degraded" });
  await expect(page.getByText("Cached events remain visible.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Refresh affected calendars" })).toBeVisible();

  await openCalendar(page, { state: "pending" });
  await expect(page.getByText("Syncing", { exact: true })).toBeVisible();

  await openCalendar(page, { state: "empty" });
  await expect(page.locator(".fc-event")).toHaveCount(0);

  if (!frozenPages.has(page)) {
    await page.clock.install({ time: new Date("2026-08-25T08:00:00Z") });
    frozenPages.add(page);
  }
  await installCalendarApiFixture(page, { state: "error" });
  await page.goto("/spa/app");
  await expect(page.getByRole("button", { name: "Try again" })).toBeVisible();

  await openCalendar(page);
  await page.getByText("Weekly planning", { exact: false }).first().click();
  await page.getByRole("button", { name: "Delete" }).click();
  await expect(page.getByRole("dialog", { name: /Choose what to delete/ })).toBeVisible();
  await expect(page.getByRole("button", { name: "Entire series" })).toBeVisible();

  await openCalendar(page, { state: "no-writable" });
  await page.getByRole("button", { name: "New event" }).first().click();
  await expect(page.getByRole("combobox")).toHaveValue("");
  await expect(page.getByRole("button", { name: "Create event" })).toBeDisabled();
});
