import { expect, test } from "@playwright/test";

test("local mock workbench serves the Calendar route without an authenticated backend", async ({ page }) => {
  await page.goto("/app");

  await expect(page.getByText("Mock data", { exact: true })).toBeVisible();
  await expect(page.getByText(/Design review/)).toBeVisible();
  await expect(page.getByRole("button", { name: "New event" }).first()).toBeEnabled();
});

test("overlapping and all-day events remain individually reachable", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto("/app");

  const designReview = page.locator(".fc-timegrid-event", { hasText: "Design review" });
  const apiReview = page.locator(".fc-timegrid-event", { hasText: "API contract review" });
  await expect(designReview).toBeVisible();
  await expect(apiReview).toBeVisible();
  const designBox = await designReview.boundingBox();
  const apiBox = await apiReview.boundingBox();
  expect(designBox).not.toBeNull();
  expect(apiBox).not.toBeNull();
  if (!designBox || !apiBox) throw new Error("Timed event bounds were not available");
  expect(designBox.x + designBox.width <= apiBox.x || apiBox.x + apiBox.width <= designBox.x).toBe(true);
  expect(Math.abs(designBox.width - apiBox.width)).toBeLessThanOrEqual(2);

  const moreLink = page.locator(".fc-more-link").first();
  await expect(moreLink).toBeVisible();
  await moreLink.click();
  await expect(page.locator(".fc-popover")).toBeVisible();
  await expect(page.locator(".fc-popover")).toContainText(/All-day/);
});

test("sync activity combines rules and runs and preserves the legacy route", async ({ page }) => {
  await page.goto("/rules");

  await expect(page.getByRole("heading", { name: "Sync activity" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Sync rules", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Recent activity" })).toBeVisible();
  await expect(page.getByText("No sync rules")).toBeVisible();
  await expect(page.getByText("No activity yet")).toBeVisible();

  await page.goto("/runs?status=run_queued");
  await expect(page).toHaveURL(/\/rules\?status=run_queued$/);
  await expect(page.getByRole("heading", { name: "Sync activity" })).toBeVisible();
  await expect(page.getByText("Run queued.")).toBeVisible();
});
