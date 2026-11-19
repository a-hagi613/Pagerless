// The JSON shapes the Portal consumes from pagerless-server. They follow the v2 Page status contract
// (see the v2 specification, "Status contract") and the auth/credentials contract in
// wayfinder/80-onboarding-cli-auth.md. Times are RFC 3339 strings.

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

export type ProvenanceConfidence = "exact" | "configured" | "identity" | "name-only" | "none";

export interface Subject {
  apiVersion: string;
  kind: TargetKind;
  // Empty only for a Node, which is cluster-scoped.
  namespace?: string;
  name: string;
  uid: string;
  // The node a workload subject is scheduled on, when the watcher knows it.
  node?: string;
  resolution: string;
  resolvedFrom?: string;
}

export interface RepositoryRef {
  url: string;
  branch: string;
  path: string;
  method: string;
  confidence: ProvenanceConfidence;
  baseCommit?: string;
}

export interface PullRequest {
  number: number;
  url: string;
  state: "draft" | "open" | "merged" | "closed";
}

export type StepKind = "tool" | "finding" | "progress" | "phase";

export interface Step {
  id: string;
  at: string;
  kind: StepKind;
  source?: string;
  tool?: string;
  summary: string;
  detail?: string;
}

export interface Finding {
  id: string;
  kind: "Log" | "Metric" | "Event" | "Trace" | "Commit" | "Object";
  source: string;
  summary: string;
  toolCallId?: string;
}

export interface DiffFile {
  path: string;
  // A server-derived unified diff. The Portal renders it; it never computes the diff itself.
  diff: string;
}

export interface ValidationCheck {
  name: string;
  passed: boolean;
  detail?: string;
}

export interface Proposal {
  action: "FilePatch" | "DiagnosisOnly";
  summary: string;
  // Bound by approval: the decision carries the hash the operator was shown.
  hash: string;
  baseCommit: string;
  branch: string;
  files: DiffFile[];
  warnings: string[];
  validation: { admission: string; checks: ValidationCheck[] };
}

export interface Decision {
  outcome: "approve" | "dismiss";
  decidedBy: string;
  decidedAt: string;
  proposalHash: string;
  reason?: string;
}

export interface Usage {
  toolCalls: number;
  modelCalls: number;
  inputTokens: number;
  outputTokens: number;
}

export interface PageSummary {
  name: string;
  phase: Phase;
  severity: Severity;
  subject: Subject;
  createdAt: string;
  triggerCount: number;
  suppressedCount?: number;
  primaryFingerprint?: string;
  // Optional: the Portal falls back to the subject when the server omits them.
  title?: string;
  signal?: string;
  repository?: RepositoryRef;
  proposalHash?: string;
  pullRequest?: PullRequest;
  terminalAt?: string;
}

export interface PageDetail extends PageSummary {
  hint?: string;
  diagnosis?: string;
  findings: Finding[];
  steps: Step[];
  proposal?: Proposal;
  decision?: Decision;
  usage?: Usage;
  verificationChecks?: ValidationCheck[];
  narrative?: string;
}

export interface WorkloadHealth {
  name: string;
  kind: TargetKind;
  ready: string;
  status: "healthy" | "degraded" | "critical";
  note?: string;
}

export interface NamespaceHealth {
  name: string;
  pods: { ready: number; total: number };
  counts: { deployments: number; statefulSets: number; cronJobs: number; persistentVolumeClaims: number };
  status: "healthy" | "degraded" | "critical";
  pages: { critical: number; warning: number; info: number };
  workloads: WorkloadHealth[];
}

export interface NodeHealth {
  name: string;
  zone: string;
  status: "ready" | "pressure";
  cpuPercent: number;
  memoryPercent: number;
  pods: number;
  conditions: string[];
}

export interface ClusterMatrix {
  context: string;
  version: string;
  pods: { ready: number; total: number };
  nodes: number;
  merged: number;
  namespaces: NamespaceHealth[];
  nodesDetail: NodeHealth[];
}

export interface PagesResponse {
  pages: PageSummary[];
  matrix: ClusterMatrix;
}

export interface MaskedValue {
  configured: boolean;
  fingerprint?: string;
  updatedAt?: string;
}

export interface AiSettings {
  baseUrl: string;
  model: string;
  apiKey: MaskedValue;
}

export interface RepositorySettings {
  owner: string;
  repo: string;
  branch: string;
  autoDraftPR: boolean;
  token: MaskedValue;
}

export type ConnectorName = "native_k8s_logs" | "loki" | "prometheus" | "cloudwatch" | "datadog";

export interface Connector {
  name: ConnectorName;
  type: "logs" | "metrics" | "alerts";
  enabled: boolean;
  url: string;
  options?: Record<string, string>;
  token: MaskedValue;
  // native_k8s_logs is mandatory and cannot be switched off.
  locked?: boolean;
  status: "ok" | "unconfigured" | "attention";
}

export interface Settings {
  managedBy: "portal" | "external";
  writeCredentials: boolean;
  version: string;
  ai: AiSettings;
  repository: RepositorySettings;
  connectors: Connector[];
  alertmanagerWebhookUrl: string;
}

export interface SettingsTestResult {
  ok: boolean;
  detail: string;
}

export interface Session {
  authenticated: boolean;
  username: string;
  mode: "token" | "disabled";
  canApprove: boolean;
  csrfToken: string;
  expiresAt: string;
}

export interface PageFilters {
  phase?: Phase;
  namespace?: string;
}

export interface NewPageRequest {
  kind: TargetKind;
  namespace?: string;
  name: string;
  hint?: string;
}

export interface DecisionRequest {
  decision: "approve" | "dismiss";
  proposalHash: string;
  reason?: string;
}
