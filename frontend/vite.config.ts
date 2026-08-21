import { defineConfig } from "vite";
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
});
