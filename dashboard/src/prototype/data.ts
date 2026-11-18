// In-memory prototype dataset. Everything a variant renders comes from here so
// the three layouts disagree only about structure, never about the story.

import type {
  ClusterNode,
  Connector,
  Evidence,
  NamespaceHealth,
  Page,
  Proposal,
  ProtoSession,
} from "./types";

export const CLUSTER = {
  name: "devbox-sandbox",
  context: "eks-devbox-sandbox",
  region: "eu-west-1",
  version: "v1.34.2",
  kpi: { namespaces: 3, pods: 32, nodes: 6, openPages: 2, verified: 12 },
};

const checkoutMemoryProposal: Proposal = {
  action: "Patch Helm values",
  title: "fix(checkout): raise checkout-api memory limit to 1Gi",
  branch: "pagerless/pg-8a4f-checkout-memory",
  path: "deploy/checkout/values.yaml",
  hash: "sha256:9f2c41b8e0d74a6fb3c15d90a2e8f6117c4bd02e5a9f38d1c6b7e4a20d3f58ca",
  baseCommit: "4a91c2d",
  risk: "low",
  confidence: 94,
  lines: [
    { kind: "meta", text: "@@ deploy/checkout/values.yaml · helm release checkout, revision 47" },
    { kind: "context", text: "api:" },
    { kind: "context", text: "  replicaCount: 3" },
    { kind: "remove", text: "  resources:" },
    { kind: "remove", text: "    limits:" },
    { kind: "remove", text: "      cpu: 500m" },
    { kind: "remove", text: "      memory: 512Mi" },
    { kind: "add", text: "  resources:" },
    { kind: "add", text: "    limits:" },
    { kind: "add", text: "      cpu: 1000m" },
    { kind: "add", text: "      memory: 1Gi" },
    { kind: "add", text: "    requests:" },
    { kind: "add", text: "      cpu: 250m" },
    { kind: "add", text: "      memory: 768Mi" },
    { kind: "context", text: "  envFrom:" },
    { kind: "context", text: "    - configMapRef:" },
    { kind: "context", text: "        name: checkout-config" },
  ],
  checks: [
    "helm template renders without warnings",
    "kubectl diff exits 0 against live release",
    "request 768Mi fits the node allocatable envelope",
  ],
};

const paymentsEnvProposal: Proposal = {
  action: "Patch Helm values",
  title: "fix(payments): correct the DATABASE_URL user name",
  branch: "pagerless/pg-2c91-payments-db-url",
  path: "deploy/payments/values.yaml",
  hash: "sha256:c7a19e5d3b8f4c26a0d9e71f5b2c8a4d6e3f901b7c5a2d8e4f6b1c90a3d5e72b",
  baseCommit: "4a91c2d",
  risk: "low",
  confidence: 98,
  lines: [
    { kind: "meta", text: "@@ deploy/payments/values.yaml · helm release payments, revision 63" },
    { kind: "context", text: "api:" },
    { kind: "context", text: "  env:" },
    {
      kind: "remove",
      text: '    DATABASE_URL: "postgres://paymnets@pg-primary.prod-payments.svc:5432/payments"',
    },
    {
      kind: "add",
      text: '    DATABASE_URL: "postgres://payments@pg-primary.prod-payments.svc:5432/payments"',
    },
    { kind: "context", text: '    DB_POOL_SIZE: "20"' },
    { kind: "context", text: "  resources:" },
    { kind: "context", text: "    limits:" },
    { kind: "context", text: "      memory: 512Mi" },
  ],
  checks: [
    "secret scan finds no credential in the diff",
    "helm template renders without warnings",
    "psql handshake verified against the primary",
  ],
};

const emailProposal: Proposal = {
  action: "Rollback deployment",
  title: "fix(email): roll back the template cache change",
  branch: "pagerless/pg-19b2-email-template-cache",
  path: "deploy/email/values.yaml",
  hash: "sha256:31bd7f0c9a4e5628b1d7c03f8e6a49b25c8d701fe3a9b4c62d5e8f01a7b3c49d",
  baseCommit: "4a91c2d",
  risk: "low",
  confidence: 96,
  lines: [
    { kind: "meta", text: "@@ deploy/email/values.yaml · helm release email, revision 88" },
    { kind: "context", text: "worker:" },
    { kind: "remove", text: "  image: ghcr.io/pagerless-sandbox/email:1.4.2" },
    { kind: "add", text: "  image: ghcr.io/pagerless-sandbox/email:1.4.1" },
    { kind: "context", text: "  templateCache:" },
    { kind: "remove", text: "    enabled: true" },
    { kind: "add", text: "    enabled: false" },
  ],
  checks: ["image tag is pinned", "rollout revision 87 restored", "error rate under 0.1 percent"],
};

const newPageProposal: Proposal = {
  action: "Patch Helm values",
  title: "fix(web): raise the web frontend readiness timeout",
  branch: "pagerless/pg-71c3-web-readiness",
  path: "deploy/web/values.yaml",
  hash: "sha256:5d8b07a1f2c94e63b0d7a821c4f95e3a6b1d0c87e2f4a95b3c6d8e01f7a2b4c6",
  baseCommit: "4a91c2d",
  risk: "low",
  confidence: 87,
  lines: [
    { kind: "meta", text: "@@ deploy/web/values.yaml · helm release web, revision 31" },
    { kind: "context", text: "frontend:" },
    { kind: "context", text: "  readinessProbe:" },
    { kind: "remove", text: "    timeoutSeconds: 2" },
    { kind: "add", text: "    timeoutSeconds: 5" },
    { kind: "context", text: "    periodSeconds: 10" },
    { kind: "context", text: "  replicaCount: 2" },
  ],
  checks: ["helm template renders without warnings", "probe budget stays inside the rollout window"],
};

const checkoutEvidence: Evidence[] = [
  {
    id: "8a4f-1",
    at: "T+00:04",
    source: "Kubernetes event",
    tool: "kubectl get events -n checkout --field-selector involvedObject.name=checkout-api-7f9c4",
    summary: "OOMKilling: container checkout-api exceeded its 512Mi memory limit",
    output: [
      "LAST SEEN   TYPE      REASON       OBJECT                              MESSAGE",
      "42s         Warning   OOMKilling   pod/checkout-api-7f9c4b8d6-x2m9p   Memory cgroup out of memory",
      "42s         Normal    Killing      pod/checkout-api-7f9c4b8d6-x2m9p   Container checkout-api failed with exit code 137",
      "8s          Warning   Unhealthy    pod/checkout-api-7f9c4b8d6-x2m9p   Readiness probe failed: connection refused",
    ],
  },
  {
    id: "8a4f-2",
    at: "T+00:11",
    source: "Prometheus",
    tool: "query container_memory_working_set_bytes{namespace=\"checkout\",container=\"checkout-api\"}",
    summary: "Working set pinned at the 512Mi ceiling for 9 minutes",
    output: [
      "t-9m   498.2 MiB   limit 512 MiB   0.94",
      "t-6m   507.9 MiB   limit 512 MiB   0.96",
      "t-3m   511.6 MiB   limit 512 MiB   0.99",
      "t-0m   OOMKilled    limit 512 MiB   —",
    ],
  },
  {
    id: "8a4f-3",
    at: "T+00:19",
    source: "Pod log",
    tool: "kubectl logs checkout-api-7f9c4b8d6-x2m9p --previous --tail=4",
    summary: "Last line before SIGKILL: report batch 42 of 128 in flight",
    output: [
      '2026-11-18T14:36:02Z info  batch.start  id=9f2c size=128',
      '2026-11-18T14:36:04Z info  batch.chunk  id=9f2c n=41 rows=10240',
      '2026-11-18T14:36:05Z info  batch.chunk  id=9f2c n=42 rows=11008',
      "2026-11-18T14:36:05Z fatal signal      SIGKILL (exit 137)",
    ],
  },
  {
    id: "8a4f-4",
    at: "T+00:26",
    source: "GitHub commit",
    tool: "gh api repos/a-hagi613/pagerless-sandbox/commits/9c31e77",
    summary: "Commit 9c31e77 (3h ago) added report batching, +18% live heap",
    output: [
      "commit 9c31e77  perf(checkout): batch invoice rows before write",
      "author  ada@example.com   2026-11-18T11:28:41Z",
      "files   internal/report/batch.go (+212 −34)",
      "note    batches are retained in memory until the final flush",
    ],
  },
  {
    id: "8a4f-5",
    at: "T+00:31",
    source: "Agent",
    tool: "diagnosis",
    summary: "Heap growth from batching after 9c31e77; the 512Mi limit is below the new steady state",
    output: [
      "The batching change retains every chunk until flush, so the heap now",
      "settles near 700–820MiB under the demo traffic profile. Restarting the",
      "pod alone recurs within 9 minutes; raising the limit to 1Gi clears the",
      "ceiling with headroom for 128-row batches. requests move to 768Mi so the",
      "scheduler reserves the working set instead of overcommitting the node.",
    ],
  },
];

const paymentsEvidence: Evidence[] = [
  {
    id: "2c91-1",
    at: "T+00:03",
    source: "Kubernetes event",
    tool: "kubectl describe pod -n prod-payments -l app=payments-api",
    summary: "Back-off restarting failed container, 12 restarts across 3 pods",
    output: [
      "Events:",
      "  Type     Reason     Age                 From     Message",
      "  Warning  BackOff    21s (x12 over 14m)  kubelet  Back-off restarting failed container api",
      "  Normal   Pulled     21s                 kubelet  Container image already present on machine",
    ],
  },
  {
    id: "2c91-2",
    at: "T+00:09",
    source: "Pod log",
    tool: "kubectl logs -n prod-payments deploy/payments-api --tail=3",
    summary: 'FATAL password authentication failed for user "paymnets"',
    output: [
      '2026-11-18T14:29:11Z info  db.connect  host=pg-primary.prod-payments.svc:5432',
      '2026-11-18T14:29:11Z fatal db.connect  FATAL: password authentication failed for user "paymnets"',
      "2026-11-18T14:29:11Z fatal api.start   database unavailable, exiting",
    ],
  },
  {
    id: "2c91-3",
    at: "T+00:15",
    source: "GitHub commit",
    tool: "gh pr list --repo a-hagi613/pagerless-sandbox --search DB_POOL_SIZE",
    summary: "PR #409 moved the URL into values.yaml and introduced the typo",
    output: [
      "409  chore(payments): move DATABASE_URL out of the sealed secret   merged 2d ago",
      "     values.yaml  DATABASE_URL: postgres://paymnets@pg-primary...",
      "     the user name was transcribed by hand during the move",
    ],
  },
  {
    id: "2c91-4",
    at: "T+00:21",
    source: "Agent",
    tool: "diagnosis",
    summary: "One-character environment regression: paymnets should be payments",
    output: [
      "The database itself accepts the password; only the user name is wrong.",
      "Correcting values.yaml restores the connection without touching the",
      "sealed secret. No data migration or restart of the primary is required.",
    ],
  },
];

const schedulingEvidence: Evidence[] = [
  {
    id: "7d10-1",
    at: "T+00:02",
    source: "Kubernetes event",
    tool: "kubectl get events -n checkout --field-selector reason=FailedScheduling",
    summary: "0/6 nodes are available: 6 Insufficient cpu",
    output: [
      "Warning  FailedScheduling  pod/report-worker-29183240-x7k2p  0/6 nodes are available:",
      "  6 Insufficient cpu. preemption: 0/6 nodes are available: 6 No preemption victims found.",
    ],
  },
  {
    id: "7d10-2",
    at: "T+00:07",
    source: "Prometheus",
    tool: "query kube_node_status_allocatable{resource=\"cpu\"}",
    summary: "Cluster CPU requests are at 97% after the batch backfill CronJob",
    output: [
      "node/ip-10-0-1-24   7.42 / 7.68 cores   96.6%",
      "node/ip-10-0-2-11   7.51 / 7.68 cores   97.8%",
      "node/ip-10-0-3-07   7.39 / 7.68 cores   96.2%",
      "pod/report-worker requests 1500m, no node has that much free",
    ],
  },
];

// The remaining evidence the scheduling Page receives while it is watched.
export const SCHEDULING_FOLLOWUP: Evidence[] = [
  {
    id: "7d10-3",
    at: "T+00:14",
    source: "Pod log",
    tool: "kubectl logs -n checkout cronjob/report-worker --tail=2",
    summary: "No container started: the Job never left Pending",
    output: [
      "pod report-worker-29183240-x7k2p Pending, 0 of 1 containers ready",
      "no log stream exists until the scheduler places the pod",
    ],
  },
  {
    id: "7d10-4",
    at: "T+00:22",
    source: "Kubernetes event",
    tool: "kubectl get nodes -o custom-columns=NAME,CPU",
    summary: "Pending action: one node is pending scale-up in the eu-west-1a group",
    output: [
      "autoscaling group eks-devbox-sandbox-a: desired 2, in service 2, pending 1",
      "the cluster autoscaler needs a further 90s before the pod can schedule",
    ],
  },
];

const emailEvidence: Evidence[] = [
  {
    id: "19b2-1",
    at: "T+00:04",
    source: "Loki",
    tool: '{app="email"} |= "error" | rate() by (pod)',
    summary: "Error rate 4.8% for 6 minutes, all pods, template cache misses",
    output: [
      "pod/email-6b9d4f7c8-j4x2n   4.81%   1.2k errors/5m",
      "pod/email-6b9d4f7c8-q8m5t   4.77%   1.2k errors/5m",
      "message: template cache lookup returned a closed renderer",
    ],
  },
  {
    id: "19b2-2",
    at: "T+00:12",
    source: "GitHub commit",
    tool: "gh api repos/a-hagi613/pagerless-sandbox/commits/77ab0d1",
    summary: "Commit 77ab0d1 enabled the template cache in the 1.4.2 image",
    output: [
      "commit 77ab0d1  perf(email): enable template cache (1.4.2)",
      "cache entries are shared across goroutines without a reload guard",
    ],
  },
  {
    id: "19b2-3",
    at: "T+00:18",
    source: "Agent",
    tool: "diagnosis",
    summary: "Rolling back 1.4.2 and disabling the cache restores 1.4.1 behaviour",
    output: [
      "The regression exists only in the 1.4.2 image; rolling back to the pinned",
      "1.4.1 tag with templateCache disabled restores the previous error budget.",
    ],
  },
  {
    id: "19b2-4",
    at: "T+00:31",
    source: "GitHub commit",
    tool: "gh pr view 412 --repo a-hagi613/pagerless-sandbox",
    summary: "PR 412 merged at 14:52; verification passed on all three pods",
    output: [
      "412  fix(email): roll back the template cache change   merged 14:52",
      "checks  rollout status deployment/email   deployment successfully rolled out",
      "        loki error rate                 0.04% (target < 0.1%)",
    ],
  },
];

export const INITIAL_PAGES: Page[] = [
  {
    id: "pg-8a4f",
    title: "checkout-api is OOMKilled during invoice batching",
    namespace: "checkout",
    workload: "checkout-api",
    kind: "Deployment",
    severity: "critical",
    status: "ready",
    openedAt: "14:36",
    openedMinutes: 6,
    trigger: "Kubernetes event",
    signal: "Exit 137 · 4 restarts in 9m · working set at 99% of limit",
    cluster: CLUSTER.name,
    owner: "unassigned",
    rootCause:
      "Report batching retains every chunk in memory until flush; the 512Mi limit is below the new steady state.",
    proposal: checkoutMemoryProposal,
    evidence: checkoutEvidence,
  },
  {
    id: "pg-2c91",
    title: "payments-api crash-loops on a database URL typo",
    namespace: "prod-payments",
    workload: "payments-api",
    kind: "Deployment",
    severity: "critical",
    status: "ready",
    openedAt: "14:29",
    openedMinutes: 14,
    trigger: "Kubernetes event",
    signal: "CrashLoopBackOff · 12 restarts · password authentication failed",
    cluster: CLUSTER.name,
    owner: "ada",
    rootCause: 'The DATABASE_URL user name reads "paymnets"; the database expects "payments".',
    proposal: paymentsEnvProposal,
    evidence: paymentsEvidence,
  },
  {
    id: "pg-7d10",
    title: "report-worker cannot be scheduled after the backfill change",
    namespace: "checkout",
    workload: "report-worker",
    kind: "CronJob",
    severity: "warning",
    status: "investigating",
    openedAt: "14:44",
    openedMinutes: 3,
    trigger: "Kubernetes event",
    signal: "FailedScheduling · 0/6 nodes have 1.5 CPU free",
    cluster: CLUSTER.name,
    owner: "unassigned",
    rootCause: "Under investigation: the node pool has no CPU headroom for the job's request.",
    evidence: schedulingEvidence,
  },
  {
    id: "pg-19b2",
    title: "email error-rate alert recovered after a rollback",
    namespace: "ingress-system",
    workload: "email",
    kind: "Deployment",
    severity: "info",
    status: "recovered",
    openedAt: "14:11",
    openedMinutes: 42,
    trigger: "Loki alert",
    signal: "Error rate 4.8% for 6m → 0.04% after PR 412 merged",
    cluster: CLUSTER.name,
    owner: "ada",
    rootCause: "The 1.4.2 template cache returns closed renderers; rolling back to 1.4.1 clears the alert.",
    proposal: emailProposal,
    evidence: emailEvidence,
    pullRequest: {
      number: 412,
      url: "https://github.com/a-hagi613/pagerless-sandbox/pull/412",
      state: "merged",
      mergedAt: "14:52",
    },
  },
];

// The stream that starts when a resource is paged from the prototype.
export const NEW_PAGE_EVIDENCE: Evidence[] = [
  {
    id: "new-2",
    at: "T+00:06",
    source: "Loki",
    tool: '{namespace="checkout",app="web"} |= "readiness" | last 5m',
    summary: "Readiness probes time out in bursts during rollout, 2s budget",
    output: [
      "14:58:02 warn rollout  probe timeout after 2.001s (attempt 3/3)",
      "14:58:12 warn rollout  probe timeout after 2.004s (attempt 3/3)",
      "14:58:22 warn rollout  probe timeout after 1.998s (attempt 3/3)",
    ],
  },
  {
    id: "new-3",
    at: "T+00:12",
    source: "Prometheus",
    tool: "histogram_quantile(0.99, http_request_duration_seconds_bucket{app=\"web\"})",
    summary: "p99 cold-start latency is 2.4s, just past the probe budget",
    output: [
      "p50 0.31s   p95 1.07s   p99 2.41s   budget 2.00s",
      "first request after rollout accounts for 92% of the tail",
    ],
  },
  {
    id: "new-4",
    at: "T+00:19",
    source: "Agent",
    tool: "diagnosis",
    summary: "Readiness timeout is below the cold-start tail; 5s clears it without slowing rollouts",
    output: [
      "Raising timeoutSeconds from 2 to 5 keeps a failing pod out of the",
      "Service while letting the first request finish. Rollout remains bounded",
      "by maxUnavailable, so the extra seconds do not extend the operation.",
    ],
  },
];

export const NAMESPACES: NamespaceHealth[] = [
  {
    name: "prod-payments",
    role: "Payment capture and ledger",
    pods: { ready: 13, total: 14 },
    counts: { deployments: 4, statefulSets: 2, cronJobs: 1, pvcs: 3 },
    status: "degraded",
    workloads: [
      { name: "payments-api", kind: "Deployment", ready: "2/3", status: "critical", note: "CrashLoopBackOff" },
      { name: "payments-worker", kind: "Deployment", ready: "3/3", status: "healthy" },
      { name: "ledger", kind: "StatefulSet", ready: "3/3", status: "healthy" },
      { name: "ledger-data", kind: "PVC", ready: "3/3", status: "healthy", note: "Bound" },
      { name: "ledger-snapshot", kind: "CronJob", ready: "1/1", status: "healthy" },
    ],
  },
  {
    name: "checkout",
    role: "Cart, pricing and invoice writes",
    pods: { ready: 9, total: 11 },
    counts: { deployments: 3, statefulSets: 1, cronJobs: 2, pvcs: 2 },
    status: "critical",
    workloads: [
      { name: "checkout-api", kind: "Deployment", ready: "2/4", status: "critical", note: "OOMKilled" },
      { name: "checkout-api-7f9c4", kind: "Pod", ready: "0/1", status: "critical", note: "OOMKilled" },
      { name: "web", kind: "Deployment", ready: "3/3", status: "healthy" },
      { name: "queue-drain", kind: "CronJob", ready: "1/1", status: "healthy" },
      { name: "report-worker", kind: "CronJob", ready: "0/1", status: "degraded", note: "Unschedulable" },
      { name: "session-cache", kind: "StatefulSet", ready: "2/2", status: "healthy" },
      { name: "uploads", kind: "PVC", ready: "1/1", status: "healthy", note: "Bound" },
    ],
  },
  {
    name: "ingress-system",
    role: "Edge routing and notification delivery",
    pods: { ready: 10, total: 10 },
    counts: { deployments: 3, statefulSets: 1, cronJobs: 1, pvcs: 1 },
    status: "healthy",
    workloads: [
      { name: "ingress-nginx", kind: "Deployment", ready: "3/3", status: "healthy" },
      { name: "email", kind: "Deployment", ready: "2/2", status: "healthy", note: "Recovered 14:52" },
      { name: "cert-renew", kind: "CronJob", ready: "1/1", status: "healthy" },
      { name: "edge-secrets", kind: "PVC", ready: "1/1", status: "healthy", note: "Bound" },
    ],
  },
];

export const NODES: ClusterNode[] = [
  { name: "ip-10-0-1-24", zone: "eu-west-1a", status: "ready", cpu: 62, memory: 71, pods: 9 },
  { name: "ip-10-0-2-11", zone: "eu-west-1a", status: "pressure", cpu: 97, memory: 84, pods: 11 },
  { name: "ip-10-0-3-07", zone: "eu-west-1b", status: "pressure", cpu: 96, memory: 78, pods: 10 },
  { name: "ip-10-0-1-58", zone: "eu-west-1b", status: "ready", cpu: 44, memory: 52, pods: 7 },
  { name: "ip-10-0-2-93", zone: "eu-west-1c", status: "ready", cpu: 38, memory: 47, pods: 8 },
  { name: "ip-10-0-3-140", zone: "eu-west-1c", status: "ready", cpu: 51, memory: 63, pods: 6 },
];

export const CONNECTORS: Connector[] = [
  {
    id: "k8s-logs",
    name: "Native Kubernetes logs",
    group: "Logs",
    state: "native",
    detail: "Streamed from the API server for every paged workload.",
    meta: "Always on",
  },
  {
    id: "loki",
    name: "Grafana Loki",
    group: "Logs",
    state: "connected",
    detail: "LogQL queries and alert rules for namespace-scoped error rates.",
    meta: "loki.devbox.internal · 4 labels",
  },
  {
    id: "prometheus",
    name: "Prometheus",
    group: "Metrics",
    state: "connected",
    detail: "PromQL for saturation, restart and probe signals.",
    meta: "prometheus.devbox.internal · 15s scrape",
  },
  {
    id: "cloudwatch",
    name: "CloudWatch",
    group: "Metrics",
    state: "available",
    detail: "Connect an AWS account to read logs and metrics from managed services.",
    meta: "Not connected",
  },
  {
    id: "datadog",
    name: "Datadog",
    group: "Metrics",
    state: "available",
    detail: "Import monitors as Page triggers alongside Prometheus alerts.",
    meta: "Not connected",
  },
  {
    id: "webhook",
    name: "Alertmanager webhook",
    group: "Alerts",
    state: "attention",
    detail: "Any Alertmanager-compatible sender can open a Page with this URL.",
    meta: "Last delivery 14:52 · 200 OK",
  },
];

export const WEBHOOK_URL = "https://pagerless.devbox.internal/api/alerts/hook_7f3a9c";

export const INITIAL_SESSION: ProtoSession = {
  role: "admin",
  name: "admin@pagerless.local",
  method: "bootstrap-token",
  expires: "in 23h",
};

export const SETTINGS_DEFAULTS = {
  aiBaseURL: "https://llm.devbox.internal/v1",
  aiModel: "qwen3-coder-30b",
  autoDraftPR: true,
  lokiConnected: true,
  prometheusConnected: true,
  cloudwatchConnected: false,
  datadogConnected: false,
} as const;

export const CLI_SECTIONS: { label: string; parity: string; commands: { command: string; note: string }[] }[] = [
  {
    label: "Pages",
    parity: "The table, filters and detail sheet mirror these verbs.",
    commands: [
      { command: "kubectl page list -n checkout --state open", note: "Same rows the Pages table renders." },
      { command: "kubectl page get pg-8a4f -o yaml", note: "Full Page including the trace and proposal." },
      { command: "kubectl page watch pg-7d10", note: "Streams Findings as the agent works." },
    ],
  },
  {
    label: "Decisions",
    parity: "Create PR and Dismiss call the same decision endpoint.",
    commands: [
      { command: "kubectl page diff pg-8a4f", note: "The unified diff shown in the drawer." },
      { command: "kubectl page approve pg-8a4f --create-pr", note: "Creates the branch and draft PR." },
      { command: 'kubectl page dismiss pg-2c91 --reason "handled in #409"', note: "Closes the Page without a PR." },
    ],
  },
  {
    label: "Setup",
    parity: "Onboarding performs these steps without a terminal.",
    commands: [
      { command: "kubectl page login --server https://pagerless.devbox.internal --token-stdin", note: "Bootstrap admin token exchange." },
      { command: "kubectl page connectors test --all", note: "Runs the connector checks shown as status pills." },
      { command: "kubectl page new --workload deployment/web -n checkout", note: "Pages a resource from the CLI." },
    ],
  },
];

export const NEW_PAGE_TEMPLATE = {
  title: "web readiness probes fail during rollout",
  namespace: "checkout",
  workload: "web",
  kind: "Deployment" as const,
  proposal: newPageProposal,
  rootCause:
    "Cold-start p99 latency is 2.4s and the readiness timeout is 2s, so the first request after each rollout fails the probe.",
  signal: "Readiness probe timeouts · p99 2.41s · 3 replicas cycling",
};
