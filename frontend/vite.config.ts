import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  base: "/spa/",
  plugins: [react()],
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
});
