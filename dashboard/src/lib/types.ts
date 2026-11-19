// The JSON shapes pagerless-server returns from /api/*. They mirror
// internal/dashboardapi and the v1alpha1 Page types; the Portal never invents a field the server does not
// send. Times are RFC 3339 strings.

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

export type Severity = "critical" | "warning" | "info";

export type TargetKind =
  | "Deployment"
  | "StatefulSet"
  | "DaemonSet"
  | "Job"
  | "CronJob"
  | "Pod"
  | "PersistentVolumeClaim"
  | "Node";

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

// TargetReference names the failing object. Namespace is empty only for a Node.
export interface TargetReference {
  apiVersion?: string;
  kind: TargetKind;
  namespace: string;
  name: string;
  uid?: string;
}

export interface Trigger {
  source: TriggerSource;
  pod?: string;
  alertFingerprint?: string;
}

export type StepKind = "ToolCall" | "Finding" | "Progress";

export interface Step {
  time: string;
  kind: StepKind;
  tool?: string;
  summary: string;
}

export type FindingKind = "Log" | "Metric" | "Trace" | "Event" | "Commit";

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

export type Action = "Scale" | "PatchResources" | "Rollback" | "NoAction";

export interface RenderedChange {
  path?: string;
  patch?: string;
}

export interface ProposalBase {
  commit?: string;
  targetGeneration?: number;
}

export interface Proposal {
  action: Action;
  parameters?: Record<string, unknown>;
  renderedChange?: RenderedChange;
  base?: ProposalBase;
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
  // Severity is the Page's own ranking. Older Pages may carry none; pageSeverity falls back to the Phase.
  severity?: Severity;
  target: TargetReference;
  trigger: Trigger;
  createdAt: string;
  startedAt?: string;
  terminalAt?: string;
  action?: Action;
  // The Page's current Proposal hash, so the inbox can offer the decision without a detail read.
  proposalHash?: string;
  pullRequest?: PullRequest;
  source: "live" | "history";
}

export interface PageDetail extends PageSummary {
  observedAt?: string;
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
  verification?: Verification;
  narrative?: string;
  grafana?: GrafanaLinks;
}

export interface PagesResponse {
  pages: PageSummary[];
}

export interface PageFilters {
  phase?: Phase;
  target?: string;
  trigger?: TriggerSource;
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
  mode: "token" | "disabled";
}

export interface TargetOption {
  kind: TargetKind;
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

export interface VersionInfo {
  version: string;
  mode: "token" | "disabled";
}

// One masked credential. The value itself never reaches the browser.
export interface MaskedField {
  configured: boolean;
  fingerprint?: string;
  updatedAt?: string;
}

export type ConnectorName = string;

export interface ConnectorSettings {
  name: ConnectorName;
  token: MaskedField;
}

export interface Settings {
  managedBy: "portal" | "external";
  // The manager's name: the external Secret that owns the values, or the Secret Pagerless owns.
  manager: string;
  // False when an external Secret manager owns the credentials.
  writeCredentials: boolean;
  // True while no AI endpoint or Target Repository is configured.
  onboarding: boolean;
  // Non-secret configuration only; credentials are never included.
  values?: Record<string, string>;
  // One masked entry per documented key.
  fields: Record<string, MaskedField>;
  ai: {
    baseUrl: string;
    model: string;
    apiKey: MaskedField;
  };
  repository: {
    owner: string;
    repo: string;
    token: MaskedField;
    apiUrl?: string;
  };
  connectors: ConnectorSettings[];
}

export interface SettingsTestResult {
  ok: boolean;
  detail: string;
}
