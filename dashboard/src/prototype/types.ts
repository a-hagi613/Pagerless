// Throwaway shapes for the Wayfinder ticket 83 UI prototype. They mirror the
// vocabulary the v2 portal is being designed around (Page, Proposal, Trace) and
// deliberately carry only what the three variants render. They are not the
// production API contract.

export type ResourceKind =
  | "Deployment"
  | "StatefulSet"
  | "Pod"
  | "Job"
  | "CronJob"
  | "PVC"
  | "Node";

export type Severity = "critical" | "warning" | "info";

export type PageStatus =
  | "pending"
  | "investigating"
  | "ready"
  | "pr-open"
  | "recovered"
  | "dismissed";

export type EvidenceSource =
  | "Kubernetes event"
  | "Pod log"
  | "Loki"
  | "Prometheus"
  | "GitHub commit"
  | "Agent";

export type DiffLineKind = "add" | "remove" | "context" | "meta";

export interface DiffLine {
  kind: DiffLineKind;
  text: string;
}

export interface Evidence {
  id: string;
  at: string;
  source: EvidenceSource;
  tool: string;
  summary: string;
  output: string[];
}

export interface Proposal {
  action: string;
  title: string;
  branch: string;
  path: string;
  hash: string;
  baseCommit: string;
  risk: "low" | "medium";
  confidence: number;
  lines: DiffLine[];
  checks: string[];
}

export interface PullRequest {
  number: number;
  url: string;
  state: "open" | "merged";
  mergedAt?: string;
}

export interface Page {
  id: string;
  title: string;
  namespace: string;
  workload: string;
  kind: ResourceKind;
  severity: Severity;
  status: PageStatus;
  openedAt: string;
  openedMinutes: number;
  trigger: string;
  signal: string;
  cluster: string;
  owner: string;
  rootCause: string;
  proposal?: Proposal;
  evidence: Evidence[];
  pullRequest?: PullRequest;
}

export interface WorkloadHealth {
  name: string;
  kind: ResourceKind;
  ready: string;
  status: "healthy" | "degraded" | "critical";
  note?: string;
}

export interface NamespaceHealth {
  name: string;
  role: string;
  pods: { ready: number; total: number };
  counts: { deployments: number; statefulSets: number; cronJobs: number; pvcs: number };
  status: "healthy" | "degraded" | "critical";
  workloads: WorkloadHealth[];
}

export interface ClusterNode {
  name: string;
  zone: string;
  status: "ready" | "pressure";
  cpu: number;
  memory: number;
  pods: number;
}

export type ConnectorState = "connected" | "available" | "attention" | "native";

export interface Connector {
  id: string;
  name: string;
  group: "Logs" | "Metrics" | "Alerts";
  state: ConnectorState;
  detail: string;
  meta: string;
}

export interface Settings {
  aiBaseURL: string;
  aiModel: string;
  autoDraftPR: boolean;
  lokiConnected: boolean;
  prometheusConnected: boolean;
  cloudwatchConnected: boolean;
  datadogConnected: boolean;
}

export type SessionRole = "admin" | "engineer" | "auditor";

export interface ProtoSession {
  role: SessionRole;
  name: string;
  method: "bootstrap-token" | "preview-token";
  expires: string;
}

export interface Toast {
  id: number;
  title: string;
  detail: string;
  tone: "success" | "info" | "warning";
}
