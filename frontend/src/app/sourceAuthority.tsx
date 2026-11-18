import { createContext, useContext } from "react";
import type { ReactNode } from "react";

type SourceStatus = { calendar_id: string; status?: string };
type Entry = { blocked: "unavailable" | "disabled" | null; revokedAt: number };

// Lives for one authenticated workspace, including route changes.
export class SourceAuthority {
  private sequence = 0;
  private version = 0;
  private active = true;
  private sources = new Map<string, Entry>();
  private listeners = new Set<() => void>();

  nextOrder() { return ++this.sequence; }
  getSnapshot = () => this.version;
  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  };
  dispose() { this.active = false; }
  observe(order: number, sources: SourceStatus[]) {
    if (!this.active) return;
    let changed = false;
    for (const source of sources) {
      if (source.status === "unavailable" || source.status === "disabled") {
        // Record when the revocation arrived, after all requests already started.
        this.sources.set(source.calendar_id, { blocked: source.status, revokedAt: ++this.sequence });
        changed = true;
      } else if (source.status === "ready") {
        const previous = this.sources.get(source.calendar_id);
        if (previous?.blocked && order > previous.revokedAt) {
          this.sources.set(source.calendar_id, { ...previous, blocked: null });
          changed = true;
        }
      }
    }
    if (!changed) return;
    this.version++;
    for (const listener of this.listeners) listener();
  }
  selection(order: number) {
    const unavailableIds = new Set<string>();
    const disabledIds = new Set<string>();
    const staleIds = new Set<string>();
    for (const [id, authority] of this.sources) {
      if (authority.blocked) unavailableIds.add(id);
      if (authority.blocked === "disabled") disabledIds.add(id);
      if (authority.revokedAt && order <= authority.revokedAt) staleIds.add(id);
    }
    return { unavailableIds, disabledIds, staleIds };
  }
  isUnavailable(id: string) { return !this.active || Boolean(this.sources.get(id)?.blocked); }
  epoch(id: string) { return this.sources.get(id)?.revokedAt ?? 0; }
  isCurrent(id: string, epoch: number) { return !this.isUnavailable(id) && epoch === this.epoch(id); }
}

const Context = createContext<SourceAuthority | null>(null);
export function SourceAuthorityProvider({ authority, children }: { authority: SourceAuthority; children: ReactNode }) {
  return <Context.Provider value={authority}>{children}</Context.Provider>;
}
export function useSourceAuthority() {
  const authority = useContext(Context);
  if (!authority) throw new Error("Source authority requires an authenticated workspace");
  return authority;
}
