import type { Plugin } from "vite";
import { createCalendarMockStore } from "./calendarMockData";

type DevRequest = AsyncIterable<Uint8Array> & { method?: string; url?: string };
type DevResponse = { statusCode: number; setHeader(name: string, value: string): void; end(body?: string): void };
type Next = () => void;

export function calendarMockPlugin(enabled: boolean): Plugin | null {
  if (!enabled) return null;
  const store = createCalendarMockStore();
  return {
    name: "calendar-local-mocks",
    apply: "serve",
    configureServer(server) {
      server.middlewares.use((request, response, next) => {
        void handleRequest(store, request as unknown as DevRequest, response as unknown as DevResponse, next);
      });
    },
  };
}

async function handleRequest(store: ReturnType<typeof createCalendarMockStore>, request: DevRequest, response: DevResponse, next: Next): Promise<void> {
  const [pathname, query = ""] = (request.url ?? "/").split("?", 2);
  if (!pathname.startsWith("/api/ui/")) return next();
  const body = await readJsonBody(request);
  const result = store.handle({ method: request.method ?? "GET", pathname, searchParams: new MockSearchParams(query), body });
  response.statusCode = result.status;
  if (result.body === undefined) return response.end();
  response.setHeader("Content-Type", "application/json; charset=utf-8");
  response.end(JSON.stringify(result.body));
}

class MockSearchParams {
  private readonly values = new Map<string, string>();

  constructor(query: string) {
    query.split("&").filter(Boolean).forEach((part) => {
      const [key, value = ""] = part.split("=", 2);
      this.values.set(decodeURIComponent(key), decodeURIComponent(value));
    });
  }

  get(name: string): string | null {
    return this.values.get(name) ?? null;
  }
}

async function readJsonBody(request: DevRequest): Promise<Record<string, unknown>> {
  if (!["POST", "PATCH", "DELETE"].includes(request.method ?? "")) return {};
  const decoder = new TextDecoder();
  let text = "";
  for await (const chunk of request) text += decoder.decode(chunk, { stream: true });
  text += decoder.decode();
  if (!text) return {};
  try {
    const body: unknown = JSON.parse(text);
    return body && typeof body === "object" && !Array.isArray(body) ? body as Record<string, unknown> : {};
  } catch {
    return {};
  }
}
