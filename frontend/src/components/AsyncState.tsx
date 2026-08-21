import { AlertCircle, CalendarDays, LoaderCircle } from "lucide-react";
import type { ReactNode } from "react";

export function LoadingState({ label = "Loading" }: { label?: string }) {
  return <div className="state-panel" role="status" aria-live="polite"><LoaderCircle className="spin state-icon" size={24} aria-hidden="true" /><span>{label}</span></div>;
}

export function EmptyState({ title, message, action }: { title: string; message: string; action?: ReactNode }) {
  return <div className="state-panel empty-state"><div className="empty-illustration"><CalendarDays size={26} /></div><h2>{title}</h2><p>{message}</p>{action}</div>;
}

export function ErrorState({ message = "Something went wrong.", retry }: { message?: string; retry?: () => void }) {
  return <div className="state-panel error-state"><div className="empty-illustration error-icon"><AlertCircle size={26} /></div><h2>Unable to load this view</h2><p>{message}</p>{retry && <button className="button button-secondary" onClick={retry}>Try again</button>}</div>;
}
