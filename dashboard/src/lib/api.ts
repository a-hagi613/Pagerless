// The API shapes mirror internal/dashboardapi's JSON responses. Times arrive as RFC 3339 strings.

export type Phase =
  | "Pending"
  | "Investigating"
  | "Proposed"
  | "Approved"
  | "Executing"
  | "Verifying"
  | "Recovered"
  | "NotRecovered"
  | "Diagnosed"
  | "Rejected"
  | "Expired"
  | "Invalid"
  | "Failed";

export type TriggerSource = "Alertmanager" | "CLI" | "Dashboard" | "FollowUp";

export type StepKind = "ToolCall" | "Finding" | "Progress";

export type FindingKind = "Log" | "Metric" | "Trace" | "Event" | "Commit";

export type Action = "RestartWorkload" | "Scale" | "PatchResources" | "Rollback" | "NoAction";

export interface WorkloadReference {
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
  base?: { commit?: string; workloadGeneration?: number };
  hash: string;
  attempt: number;
  expiresAt: string;
}

export interface Decision {
  outcome: "Approved" | "Rejected";
  approver: string;
  reason?: string;
  decidedAt: string;
}

export interface Execution {
  pullRequestURL?: string;
  mergeCommit?: string;
  restartedPods?: string[];
  executedAt?: string;
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

export interface TriageSummary {
  name: string;
  namespace: string;
  phase: Phase;
  workload: WorkloadReference;
  trigger: Trigger;
  createdAt: string;
  startedAt?: string;
  terminalAt?: string;
  action?: Action;
  followUpTriage?: string;
  source: "live" | "history";
}

export interface TriageDetail {
  name: string;
  namespace: string;
  phase: Phase;
  source: "live" | "history";
  createdAt: string;
  startedAt?: string;
  terminalAt?: string;
  observedAt?: string;
  workload: WorkloadReference;
  trigger: Trigger;
  hint?: string;
  followsTriage?: string;
  triggerCount: number;
  steps: Step[];
  stepCount: number;
  toolCallCount: number;
  usage?: Usage;
  findings: Finding[];
  diagnosis?: string;
  proposal?: Proposal;
  decision?: Decision;
  execution?: Execution;
  verification?: Verification;
  narrative?: string;
  followUpTriage?: string;
  grafana?: GrafanaLinks;
}

export interface TriageFilters {
  phase?: string;
  workload?: string;
  trigger?: string;
  since?: string;
}

interface ListResponse {
  triages: TriageSummary[];
}

interface ErrorResponse {
  error?: string;
}

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    headers: { Accept: "application/json" },
    signal,
  });
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

export async function listTriages(
  filters: TriageFilters,
  signal?: AbortSignal,
): Promise<TriageSummary[]> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value !== undefined && value !== "") {
      query.set(key, value);
    }
  }
  const encoded = query.toString();
  const response = await getJSON<ListResponse>(
    encoded === "" ? "/api/triages" : `/api/triages?${encoded}`,
    signal,
  );
  return response.triages;
}

export async function getTriage(name: string, signal?: AbortSignal): Promise<TriageDetail> {
  return await getJSON<TriageDetail>(`/api/triages/${encodeURIComponent(name)}`, signal);
}

export function eventsURL(name: string): string {
  return `/api/triages/${encodeURIComponent(name)}/events`;
}
