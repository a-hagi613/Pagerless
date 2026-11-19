// The API shapes mirror internal/dashboardapi's JSON responses. Times arrive as RFC 3339 strings.

export type Phase =
  | "Pending"
  | "Investigating"
  | "Proposed"
  | "Approved"
  | "PullRequestOpen"
  | "Verifying"
  | "Recovered"
  | "NotRecovered"
  | "Diagnosed"
  | "Rejected"
  | "Expired"
  | "Invalid"
  | "Superseded"
  | "Failed";

export type TriggerSource =
  | "Event"
  | "Condition"
  | "Alertmanager"
  | "Grafana"
  | "Datadog"
  | "SNS"
  | "CLI"
  | "Portal"
  | "API";

export type StepKind = "ToolCall" | "Finding" | "Progress";

export type FindingKind = "Log" | "Metric" | "Trace" | "Event" | "Commit";

export type Action = "Scale" | "PatchResources" | "Rollback" | "NoAction";

export interface TargetReference {
  kind: string;
  namespace: string;
  name: string;
}

export interface Trigger {
  source: TriggerSource;
  pod?: string;
  alertFingerprint?: string;
}

export interface Step {
  time: string;
  kind: StepKind;
  tool?: string;
  summary: string;
}

export interface Finding {
  kind: FindingKind;
  source: string;
  summary: string;
}

export interface Usage {
  inputTokens: number;
  outputTokens: number;
  costUSD: number;
}

export interface RenderedChange {
  path?: string;
  patch?: string;
}

export interface Proposal {
  action: Action;
  parameters?: Record<string, unknown>;
  renderedChange?: RenderedChange;
  base?: { commit?: string; targetGeneration?: number };
  hash: string;
  attempt: number;
  expiresAt: string;
}

export interface Decision {
  outcome: "Approved" | "Rejected";
  proposalHash: string;
  decidedBy: string;
  reason?: string;
  decidedAt: string;
}

export interface PullRequest {
  provider?: string;
  number?: number;
  url?: string;
  branch?: string;
  headSHA?: string;
  state?: "Draft" | "Open" | "Merged" | "Closed";
  checks?: "Pending" | "Passing" | "Failing" | "Unknown";
  mergeCommit?: string;
  openedAt?: string;
  mergedAt?: string;
}

export interface VerificationCheck {
  name: string;
  passed: boolean;
  message?: string;
}

export interface Verification {
  startedAt?: string;
  checks: VerificationCheck[];
}

export interface DashboardLink {
  embedURL: string;
  deepLink: string;
}

export interface GrafanaLinks {
  baseURL: string;
  metrics?: DashboardLink;
  logs?: DashboardLink;
}

export interface PageSummary {
  name: string;
  namespace: string;
  phase: Phase;
  target: TargetReference;
  trigger: Trigger;
  createdAt: string;
  startedAt?: string;
  terminalAt?: string;
  action?: Action;
  source: "live" | "history";
}

export interface PageDetail {
  name: string;
  namespace: string;
  phase: Phase;
  source: "live" | "history";
  createdAt: string;
  startedAt?: string;
  terminalAt?: string;
  observedAt?: string;
  target: TargetReference;
  trigger: Trigger;
  hint?: string;
  previousPage?: string;
  triggerCount: number;
  steps: Step[];
  stepCount: number;
  toolCallCount: number;
  usage?: Usage;
  findings: Finding[];
  diagnosis?: string;
  proposal?: Proposal;
  decision?: Decision;
  pullRequest?: PullRequest;
  verification?: Verification;
  narrative?: string;
  grafana?: GrafanaLinks;
}

export interface PageFilters {
  phase?: string;
  target?: string;
  trigger?: string;
  since?: string;
}

export interface Session {
  authenticated: boolean;
  username: string;
  subject: string;
  groups: string[];
  canApprove: boolean;
  csrfToken: string;
  expiresAt: string;
}

export interface TargetOption {
  kind: "Deployment" | "StatefulSet";
  namespace: string;
  name: string;
}

export interface DecisionAnswer {
  name: string;
  page: string;
  decision: "Approved" | "Rejected";
  decidedBy: string;
}

export interface StartAnswer {
  name: string;
  alreadyActive: boolean;
}

interface ListResponse {
  pages: PageSummary[];
}

interface TargetListResponse {
  targets: TargetOption[];
}

interface ErrorResponse {
  error?: string;
}

// The session's CSRF token, which every write repeats in the X-CSRF-Token header.
let csrfToken = "";

export function setCSRFToken(token: string): void {
  csrfToken = token;
}

async function requestJSON<T>(
  path: string,
  init: RequestInit = {},
  signal?: AbortSignal,
): Promise<T> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }
  if (method !== "GET" && method !== "HEAD") {
    headers.set("X-CSRF-Token", csrfToken);
  }
  const response = await fetch(path, {
    ...init,
    headers,
    signal,
    credentials: "same-origin",
  });
  if (response.status === 401) {
    // The session expired or was revoked. The server's sign-in route starts the flow.
    window.location.assign("/login");
    throw new Error("the session has ended; sign in again");
  }
  if (!response.ok) {
    let message = `${String(response.status)} ${response.statusText}`;
    try {
      const body = (await response.json()) as ErrorResponse;
      if (body.error !== undefined) {
        message = body.error;
      }
    } catch {
      // The body is not JSON; keep the status line.
    }
    throw new Error(message);
  }
  return (await response.json()) as T;
}

export async function getSession(signal?: AbortSignal): Promise<Session> {
  const session = await requestJSON<Session>("/api/session", {}, signal);
  setCSRFToken(session.csrfToken);
  return session;
}

export async function listPages(
  filters: PageFilters,
  signal?: AbortSignal,
): Promise<PageSummary[]> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value !== undefined && value !== "") {
      query.set(key, value);
    }
  }
  const encoded = query.toString();
  const response = await requestJSON<ListResponse>(
    encoded === "" ? "/api/pages" : `/api/pages?${encoded}`,
    {},
    signal,
  );
  return response.pages;
}

export async function getPage(name: string, signal?: AbortSignal): Promise<PageDetail> {
  return await requestJSON<PageDetail>(`/api/pages/${encodeURIComponent(name)}`, {}, signal);
}

export async function decidePage(
  name: string,
  decision: "Approved" | "Rejected",
  reason: string,
): Promise<DecisionAnswer> {
  const body: Record<string, string> = { decision };
  if (reason !== "") {
    body.reason = reason;
  }
  return await requestJSON<DecisionAnswer>(
    `/api/pages/${encodeURIComponent(name)}/decision`,
    { method: "POST", body: JSON.stringify(body) },
  );
}

export async function listTargets(signal?: AbortSignal): Promise<TargetOption[]> {
  const response = await requestJSON<TargetListResponse>("/api/targets", {}, signal);
  return response.targets;
}

export async function startPage(target: TargetOption, hint: string): Promise<StartAnswer> {
  const body: { target: TargetOption; hint?: string } = { target };
  if (hint !== "") {
    body.hint = hint;
  }
  return await requestJSON<StartAnswer>("/api/pages", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export async function signOut(): Promise<void> {
  await fetch("/logout", {
    method: "POST",
    headers: { "X-CSRF-Token": csrfToken },
    credentials: "same-origin",
  });
  window.location.assign("/signed-out");
}

export function eventsURL(name: string): string {
  return `/api/pages/${encodeURIComponent(name)}/events`;
}
