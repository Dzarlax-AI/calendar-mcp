import { createContext, lazy, Suspense, useContext, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { createBrowserRouter, Navigate, Outlet, RouterProvider, NavLink, useLocation, useRouteError } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { CalendarDays, Link2, ListChecks, LoaderCircle, Menu, PlayCircle, Settings2, ShieldAlert, X } from "lucide-react";
import { getBootstrap, isSessionExpiredError, navigateToApp } from "../lib/api";
import type { Bootstrap } from "../lib/types";
import "../styles/app.css";

const CalendarPage = lazy(() => import("../features/calendar/CalendarPage"));
const ControlPlanePage = lazy(() => import("../features/control-plane/ControlPlanePage"));
const DiagnosticsPage = lazy(() => import("../features/diagnostics/DiagnosticsPage"));

export function useBootstrap() {
  return useQuery({ queryKey: ["bootstrap"], queryFn: getBootstrap, retry: (failureCount, error) => !isSessionExpiredError(error) && failureCount < 2 });
}

const navItems = [
  { to: "/app", label: "Calendar", icon: CalendarDays, end: true },
  { to: "/connections", label: "Connections", icon: Link2 },
  { to: "/rules", label: "Sync Rules", icon: ListChecks },
  { to: "/runs", label: "Runs", icon: PlayCircle },
  { to: "/settings", label: "Settings", icon: Settings2 },
];

function SuspendedRoute() {
  return <div className="route-loading" role="status" aria-live="polite"><LoaderCircle className="spin" size={22} aria-hidden="true" /><span>Loading view</span></div>;
}

export function RouteErrorPage() {
  useRouteError();
  return <div className="app-gate" role="alert">
    <div className="empty-illustration"><CalendarDays size={30} /></div>
    <h1>Calendar couldn't finish loading</h1>
    <p>Refresh the page to load the latest version of the calendar.</p>
    <button className="button button-primary" onClick={() => window.location.reload()}>Refresh calendar</button>
  </div>;
}

function Sidebar({ open, onClose, username, diagnosticsOperator }: { open: boolean; onClose: () => void; username?: string; diagnosticsOperator?: boolean }) {
  const accountLabel = username?.trim() || "Calendar account";
  return <aside className={`app-sidebar ${open ? "is-open" : ""}`} aria-label="Primary navigation">
    <div className="brand-row">
      <CalendarDays size={25} strokeWidth={2.2} />
      <span>Calendar</span>
      <button className="icon-button sidebar-close" onClick={onClose} aria-label="Close navigation"><X size={20} /></button>
    </div>
    <nav className="primary-nav">
      {navItems.concat({ to: "/diagnostics", label: "Diagnostics", icon: ShieldAlert, end: false }).filter((item) => item.to !== "/diagnostics" || diagnosticsOperator === true).map(({ to, label, icon: Icon, end }) => <NavLink key={to} to={to} end={end} onClick={onClose} className={({ isActive }) => `nav-link ${isActive ? "is-active" : ""}`}>
        <Icon size={19} strokeWidth={1.9} /><span>{label}</span>
      </NavLink>)}
    </nav>
    <div className="sidebar-bottom">
      <div className="sidebar-note"><span className="status-dot" /> <span>Synced calendars stay in one view.</span></div>
      <div className="user-chip"><div className="avatar">{initials(accountLabel)}</div><div className="user-copy"><strong>{accountLabel}</strong><span>Control plane</span></div><span className="user-caret">⌄</span></div>
    </div>
  </aside>;
}

function initials(value: string) {
  return value.split(/\s+/).map((part) => part[0]).join("").slice(0, 2).toUpperCase();
}

function Header({ onMenu }: { onMenu: () => void }) {
  return <header className="mobile-header">
    <button className="icon-button" onClick={onMenu} aria-label="Open navigation"><Menu size={21} /></button>
    <div className="mobile-brand"><CalendarDays size={21} /><strong>Calendar</strong></div>
  </header>;
}

function BootstrapGate() {
  const bootstrap = useBootstrap();
  useEffect(() => {
    if (isSessionExpiredError(bootstrap.error)) navigateToApp();
  }, [bootstrap.error]);
  if (bootstrap.isPending) return <div className="app-gate" role="status" aria-live="polite"><LoaderCircle className="spin" size={26} aria-hidden="true" /><p>Loading your calendar</p></div>;
  if (bootstrap.isError) return <div className="app-gate"><div className="empty-illustration"><CalendarDays size={30} /></div><h1>Calendar is unavailable</h1><p>We couldn't load the authenticated calendar workspace. Try refreshing the page.</p><button className="button button-primary" onClick={() => void bootstrap.refetch()}>Try again</button></div>;
  return <Workspace bootstrap={bootstrap.data} />;
}

function Workspace({ bootstrap }: { bootstrap: Bootstrap }) {
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const location = useLocation();
  const calendarRoute = location.pathname === "/app";
  return <div className="app-frame">
    {!calendarRoute && <Sidebar open={sidebarOpen} onClose={() => setSidebarOpen(false)} username={bootstrap.username} diagnosticsOperator={bootstrap.diagnostics_operator} />}
    {!calendarRoute && sidebarOpen && <button className="sidebar-scrim" aria-label="Close navigation" onClick={() => setSidebarOpen(false)} />}
    <div className="app-content">{!calendarRoute && <Header onMenu={() => setSidebarOpen(true)} />}<BootstrapContext.Provider value={bootstrap}><Suspense fallback={<SuspendedRoute />}><Outlet /></Suspense></BootstrapContext.Provider></div>
  </div>;
}

const BootstrapContext = createContext<Bootstrap | null>(null);
export function useBootstrapData() {
  const value = useContext(BootstrapContext);
  if (!value) throw new Error("useBootstrapData must be used inside the authenticated workspace");
  return value;
}

function Root() { return <BootstrapGate />; }

const router = createBrowserRouter([
  { path: "/", element: <Navigate to="/app" replace /> },
  {
    element: <Root />,
    errorElement: <RouteErrorPage />,
    children: [
      { path: "/app", element: <CalendarPage /> },
      { path: "/connections", element: <ControlPlanePage section="connections" /> },
      { path: "/rules", element: <ControlPlanePage section="rules" /> },
      { path: "/rules/new", element: <ControlPlanePage section="rule-new" /> },
      { path: "/runs", element: <ControlPlanePage section="runs" /> },
      { path: "/settings", element: <ControlPlanePage section="settings" /> },
      { path: "/diagnostics", element: <DiagnosticsPage /> },
      { path: "*", element: <Navigate to="/app" replace /> },
    ],
  },
]);

export default function App() { return <RouterProvider router={router} />; }
