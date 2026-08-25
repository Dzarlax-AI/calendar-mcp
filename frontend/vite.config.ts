import { defineConfig } from "vitest/config";
import { loadEnv } from "vite";
import react from "@vitejs/plugin-react";
import { calendarMockPlugin } from "./src/dev/calendarMockServer";

export default defineConfig(({ command, mode }) => {
  const processEnv = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env;
  const calendarMocksEnabled = (processEnv?.VITE_CALENDAR_MOCKS ?? loadEnv(mode, ".", "VITE_").VITE_CALENDAR_MOCKS) === "true";
  return {
  base: command === "serve" && calendarMocksEnabled ? "/" : "/spa/",
  plugins: [react(), calendarMockPlugin(command === "serve" && calendarMocksEnabled)].filter((plugin): plugin is NonNullable<typeof plugin> => plugin !== null),
  build: {
    rollupOptions: {
      output: {
        manualChunks: {
          calendar: [
            "@fullcalendar/react",
            "@fullcalendar/daygrid",
            "@fullcalendar/timegrid",
            "@fullcalendar/list",
            "@fullcalendar/interaction",
          ],
          query: ["@tanstack/react-query"],
        },
      },
    },
  },
  test: {
    exclude: ["e2e/**", "node_modules/**", "dist/**"],
  },
};
});
