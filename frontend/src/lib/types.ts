export type Provider = "google" | "microsoft" | "apple" | string;

export type CalendarCapability = {
  read: boolean;
  create: boolean;
  write: boolean;
  delete: boolean;
  recurring: boolean;
  recurrenceScopes?: Array<"single" | "following" | "series">;
};

export type CalendarRecord = {
  id: string;
  name: string;
  provider: Provider;
  accountLabel?: string;
  timezone?: string;
  group?: string;
  color: string;
  readOnly?: boolean;
  capability: CalendarCapability;
};

export type Connection = {
  id: string;
  provider: Provider;
  label: string;
  email?: string;
  status: "connected" | "needs_attention" | "disconnected" | string;
  connectURL?: string;
  canReconnect?: boolean;
};

export type SyncRule = {
  id: string;
  name?: string;
  source?: string;
  destination?: string;
  sourceCalendarId?: string;
  targetCalendarId?: string;
  state?: string;
  enabled: boolean;
  lastRun?: string;
};

export type RunRecord = {
  id: string;
  ruleId?: string;
  status: "queued" | "running" | "success" | "failed" | string;
  startedAt?: string;
  finishedAt?: string;
  message?: string;
  outcome?: string;
  trigger?: string;
  createdCount?: number;
  updatedCount?: number;
  deletedCount?: number;
};

export type Settings = {
  email?: string;
  publicURL?: string;
  mcpKeyConfigured?: boolean;
  timezone?: string;
  mcpEndpoint?: string;
  legacyApiKeyConfigured?: boolean;
};

export type Bootstrap = {
  csrf_token: string;
  username?: string;
  calendars: CalendarRecord[];
  connections: Connection[];
  rules: SyncRule[];
  runs: RunRecord[];
  settings: Settings;
  capabilities?: Record<string, RawCalendarCapabilities>;
  diagnostics_operator?: boolean;
};

export type SyncDiagnosticObject = {
  object_id: string;
  etag?: string;
  error_code?: string;
  first_seen_at?: string;
  last_seen_at?: string;
  next_repair_at?: string;
  repair_attempts?: number;
  artifact_available?: boolean;
};

export type SyncDiagnosticCalendar = {
  calendar_id: string;
  display_name: string;
  provider: string;
  status: string;
  active_quarantine_count: number;
  last_success_at?: string | null;
  last_error?: string | null;
  next_repair_at?: string | null;
  objects: SyncDiagnosticObject[];
};

export type SyncDiagnostics = {
  summary: { degraded_calendars: number; active_objects: number; artifacts_available: number };
  calendars: SyncDiagnosticCalendar[];
};

export type SyncArtifact = {
  calendar_id: string;
  object_id: string;
  etag?: string;
  payload_base64: string;
  payload_sha256: string;
  content_type?: string;
  provider_status?: number;
  provider_reason?: string;
  truncated: boolean;
  captured_at: string;
  expires_at: string;
};

export type RawCalendarCapabilities = {
  read_only?: boolean;
  operations?: { list?: boolean; get?: boolean; create?: boolean; update?: boolean; delete?: boolean };
  mutation_scopes?: Array<"single" | "following" | "series">;
};

export type EventTime = { date?: string; date_time?: string; time_zone?: string };

export type RecurrenceInfo = {
  isRecurring: boolean;
  masterId?: string;
  occurrenceStart?: string;
  scopes?: Array<"single" | "following" | "series">;
};

export type EventRecord = {
  id: string;
  calendarId: string;
  title: string;
  description?: string;
  location?: string;
  start: string;
  end: string;
  allDay: boolean;
  timezone?: string;
  etag?: string;
  readOnly?: boolean;
  source?: string;
  recurrence?: RecurrenceInfo;
  originalStart?: EventTime;
  warnings?: string[];
};

export type EventSourceStatus = {
  provider: string;
  calendar_id: string;
  complete: boolean;
  error?: string;
  status?: "pending" | "syncing" | "ready" | "failed" | "parked" | "degraded" | string;
  last_success_at?: string | null;
  stale?: boolean;
  error_code?: string | null;
};

export type EventListResponse = {
  items: EventRecord[];
  sources?: EventSourceStatus[];
  complete: boolean;
};

export type EventDraft = {
  title: string;
  description: string;
  location: string;
  start: string;
  end: string;
  allDay: boolean;
};

export type EventCreateRequest = {
  calendar_id: string;
  title: string;
  description?: string;
  location?: string;
  start: EventTime;
  end: EventTime;
  scope?: "single" | "following" | "series";
};

export type EventUpdateRequest = {
  title?: string;
  description?: string;
  location?: string;
  start?: EventTime;
  end?: EventTime;
  scope?: "single" | "following" | "series";
  expected_etag?: string;
  effective_from?: EventTime;
};
