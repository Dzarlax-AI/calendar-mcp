import { defineConfig, devices } from "@playwright/test";

const isCI = Boolean(process.env.CI);
const useMockServer = process.env.E2E_MOCK_SERVER === "true";

export default defineConfig({
  testDir: "./e2e",
  testIgnore: useMockServer ? undefined : ["calendar.mock-server.spec.ts"],
  fullyParallel: true,
  forbidOnly: isCI,
  retries: isCI ? 2 : 0,
  workers: isCI ? 1 : undefined,
  reporter: [
    ["list"],
    ["html", { outputFolder: "playwright-report", open: "never" }],
  ],
  expect: {
    toHaveScreenshot: {
      animations: "disabled",
      caret: "hide",
      scale: "css",
    },
  },
  use: {
    ...devices["Desktop Chrome"],
    baseURL: "http://127.0.0.1:4173",
    colorScheme: "light",
    locale: "en-US",
    reducedMotion: "reduce",
    timezoneId: "UTC",
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
    video: "off",
  },
  webServer: {
    command: useMockServer
      ? "VITE_CALENDAR_MOCKS=true npm run dev -- --host 127.0.0.1 --port 4173 --strictPort"
      : "npm run build && ./node_modules/.bin/vite preview --host 127.0.0.1 --port 4173 --strictPort",
    url: useMockServer ? "http://127.0.0.1:4173/app" : "http://127.0.0.1:4173/spa/app",
    reuseExistingServer: !isCI && !useMockServer,
    timeout: 120_000,
  },
});
