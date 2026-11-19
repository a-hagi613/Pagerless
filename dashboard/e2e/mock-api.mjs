// An offline stand-in for pagerless-server, used only by the Playwright suite. The production build
// talks to the real /api/*; nothing in src/ imports this file. It serves deterministic fixtures and a
// real Server-Sent Events trace so the browser exercises the same transport the cluster does.

import { createServer } from "node:http";

const port = Number(process.env.MOCK_API_PORT ?? "8092");
const sessionCookie = "pagerless_session=test-session";
const csrfToken = "test-csrf-token";
const started = Date.now();
const iso = (offsetMs = 0) => new Date(started - offsetMs).toISOString();

const traceSteps = [
  {
    id: "step-1",
    at: iso(280_000),
    kind: "tool",
    tool: "pods",
    source: "Kubernetes API",
    summary: "Read 3 pods for deployment/shop/checkout",
    detail: "NAME                        READY   STATUS             RESTARTS\ncheckout-6c9f7b8d4-2xkq    0/1     CrashLoopBackOff   7\ncheckout-6c9f7b8d4-8m4p    1/1     Running            0",
  },
  {
    id: "step-2",
    at: iso(240_000),
    kind: "tool",
    tool: "logs",
    source: "Kubernetes API",
    summary: "Read previous-container logs (tail 80)",
    detail: "panic: readiness probe /healthz returned 500 after 2.01s",
  },
  {
    id: "step-3",
    at: iso(180_000),
    kind: "finding",
    source: "Prometheus",
    summary: "p99 latency is 2.41s against a 2s readiness timeout",
  },
];

const liveSteps = [
  {
    id: "step-live-1",
    at: iso(30_000),
    kind: "progress",
    summary: "Cross-checking the previous Deployment revision",
  },
  {
    id: "step-live-2",
    at: iso(10_000),
    kind: "finding",
    source: "GitHub commit",
    summary: "Commit 4a7c1e2 raised the memory limit and dropped the startup delay",
  },
];

const proposal = {
  action: "FilePatch",
  summary: "Raise the readiness timeout to 4s",
  hash: "sha256:1f4c9ab27d3e5a6b8c0d1e2f3a4b5c6d7e8f9012",
  baseCommit: "9b2f4c1e7d8a3b5c6d0e1f2a3b4c5d6e7f80912",
  branch: "pagerless/checkout-7d9f-1f4c9ab2",
  files: [
    {
      path: "apps/shop/checkout/deployment.yaml",
      diff: [
        "diff --git a/apps/shop/checkout/deployment.yaml b/apps/shop/checkout/deployment.yaml",
        "--- a/apps/shop/checkout/deployment.yaml",
        "+++ b/apps/shop/checkout/deployment.yaml",
        "@@ -24,7 +24,7 @@ spec:",
        "       containers:",
        "         - name: checkout",
        "           readinessProbe:",
        "-            timeoutSeconds: 2",
        "+            timeoutSeconds: 4",
        "             httpGet:",
        "               path: /healthz",
        "               port: 8080",
      ].join("\n"),
    },
  ],
  warnings: [],
  validation: {
    admission: "admission: not evaluated — CI is authoritative",
    checks: [
      { name: "kind allowlist", passed: true },
      { name: "identity preserved", passed: true },
      { name: "openapi/v3 structural validation", passed: true },
    ],
  },
};

const pages = [
  {
    name: "checkout-7d9f",
    phase: "Proposed",
    severity: "critical",
    subject: {
      apiVersion: "apps/v1",
      kind: "Deployment",
      namespace: "shop",
      name: "checkout",
      uid: "uid-checkout",
      node: "ip-10-0-1-17",
      resolution: "direct",
    },
    createdAt: iso(360_000),
    triggerCount: 7,
    suppressedCount: 12,
    title: "checkout readiness probe times out during rollout",
    signal: "Readiness probe timeouts · p99 2.41s · 3 replicas cycling",
    repository: {
      url: "github.com/a-hagi613/pagerless-sandbox",
      branch: "main",
      path: "apps/shop/checkout/deployment.yaml",
      method: "argocd tracking-id",
      confidence: "exact",
    },
    proposalHash: proposal.hash,
    diagnosis:
      "Cold-start p99 is 2.41s while the readiness probe allows 2s, so the first request after each rollout fails and the Deployment never becomes available.",
    findings: [
      { id: "f-1", kind: "Log", source: "previous container", summary: "panic: readiness probe returned 500 after 2.01s" },
      { id: "f-2", kind: "Metric", source: "Prometheus", summary: "p99 latency 2.41s over the last 15 minutes" },
      { id: "f-3", kind: "Commit", source: "GitHub", summary: "Commit 4a7c1e2 removed the 5s startup sleep" },
    ],
    steps: traceSteps,
    proposal,
    usage: { toolCalls: 9, modelCalls: 3, inputTokens: 41_923, outputTokens: 2_817 },
    narrative: "The probe timeout is shorter than the measured cold-start latency.",
  },
  {
    name: "payments-api-2xkq",
    phase: "Proposed",
    severity: "warning",
    subject: {
      apiVersion: "apps/v1",
      kind: "Deployment",
      namespace: "payments",
      name: "payments-api",
      uid: "uid-payments",
      node: "ip-10-0-2-21",
      resolution: "direct",
    },
    createdAt: iso(540_000),
    triggerCount: 3,
    title: "payments-api is OOMKilled under the default memory limit",
    signal: "OOMKilled 3 times · working set 512Mi against a 384Mi limit",
    repository: {
      url: "github.com/a-hagi613/pagerless-sandbox",
      branch: "main",
      path: "apps/payments/api/deployment.yaml",
      method: "helm release metadata",
      confidence: "configured",
    },
    proposalHash: "sha256:8d2c5ab17e3f4a5b6c7d8e9f0a1b2c3d4e5f6071",
    diagnosis: "The measured working set at peak is 512Mi while the limit is 384Mi, so the kubelet evicts the container during settlement batches.",
    findings: [{ id: "f-4", kind: "Event", source: "Kubernetes API", summary: "OOMKilled 3 times in 20 minutes" }],
    steps: traceSteps,
    proposal: {
      ...proposal,
      summary: "Raise the memory limit to 640Mi",
      hash: "sha256:8d2c5ab17e3f4a5b6c7d8e9f0a1b2c3d4e5f6071",
      branch: "pagerless/payments-api-2xkq-8d2c5ab1",
      files: [
        {
          path: "apps/payments/api/deployment.yaml",
          diff: [
            "@@ -31,7 +31,7 @@ spec:",
            "           resources:",
            "             limits:",
            "-              memory: 384Mi",
            "+              memory: 640Mi",
            "             requests:",
            "               memory: 256Mi",
          ].join("\n"),
        },
      ],
      warnings: ["resources-removed"],
    },
  },
  {
    name: "report-worker-4m1p",
    phase: "Investigating",
    severity: "warning",
    subject: {
      apiVersion: "apps/v1",
      kind: "Deployment",
      namespace: "shop",
      name: "report-worker",
      uid: "uid-report-worker",
      node: "ip-10-0-1-17",
      resolution: "direct",
    },
    createdAt: iso(90_000),
    triggerCount: 1,
    title: "report-worker is restarting every ninety seconds",
    signal: "CrashLoopBackOff · 4 restarts · exit code 137",
    diagnosis: undefined,
    findings: [],
    steps: traceSteps.slice(0, 2),
  },
  {
    name: "ip-10-0-2-21",
    phase: "Diagnosed",
    severity: "critical",
    subject: {
      apiVersion: "v1",
      kind: "Node",
      name: "ip-10-0-2-21",
      uid: "uid-node-2",
      resolution: "cluster",
    },
    createdAt: iso(900_000),
    triggerCount: 2,
    title: "node ip-10-0-2-21 reports MemoryPressure",
    signal: "MemoryPressure=True · 94% memory allocated",
    diagnosis:
      "The node is above its memory eviction threshold. Drain it and move report-worker to a node with headroom; no file change can fix a node.",
    findings: [{ id: "f-5", kind: "Object", source: "Kubernetes API", summary: "MemoryPressure=True since 14:52" }],
    steps: traceSteps.slice(0, 1),
  },
  {
    name: "image-sync-9k3d",
    phase: "PullRequestOpen",
    severity: "info",
    subject: {
      apiVersion: "apps/v1",
      kind: "CronJob",
      namespace: "platform",
      name: "image-sync",
      uid: "uid-image-sync",
      node: "ip-10-0-1-17",
      resolution: "direct",
    },
    createdAt: iso(1_800_000),
    triggerCount: 2,
    title: "image-sync failed twice on a transient registry timeout",
    signal: "BackoffLimitExceeded · registry returned 503",
    repository: {
      url: "github.com/a-hagi613/pagerless-sandbox",
      branch: "main",
      path: "apps/platform/image-sync/cronjob.yaml",
      method: "flux kustomization",
      confidence: "identity",
    },
    proposalHash: "sha256:2b7c9d1e3f5a7b9c1d3e5f7a9b1c3d5e7f9a1b3c",
    pullRequest: { number: 418, url: "https://github.com/a-hagi613/pagerless-sandbox/pull/418", state: "draft" },
    diagnosis: "The registry returned 503 on the first two attempts; retrying with a backoff is enough.",
    findings: [],
    steps: traceSteps.slice(0, 2),
    proposal: { ...proposal, summary: "Add a backoffLimit of 4", hash: "sha256:2b7c9d1e3f5a7b9c1d3e5f7a9b1c3d5e7f9a1b3c" },
  },
  {
    name: "search-indexer-8h2n",
    phase: "Recovered",
    severity: "info",
    subject: {
      apiVersion: "apps/v1",
      kind: "StatefulSet",
      namespace: "payments",
      name: "search-indexer",
      uid: "uid-search-indexer",
      node: "ip-10-0-2-21",
      resolution: "direct",
    },
    createdAt: iso(7_200_000),
    terminalAt: iso(1_200_000),
    triggerCount: 1,
    title: "search-indexer recovered after the merged memory limit",
    signal: "Recovered · no restarts for 30 minutes",
    diagnosis: "The merged memory limit removed the eviction pressure.",
    findings: [],
    steps: traceSteps.slice(0, 2),
  },
];

const matrix = {
  context: "kind-pagerless",
  version: "v1.34.0",
  pods: { ready: 58, total: 62 },
  nodes: 2,
  merged: 3,
  namespaces: [
    {
      name: "shop",
      pods: { ready: 22, total: 26 },
      counts: { deployments: 4, statefulSets: 0, cronJobs: 1, persistentVolumeClaims: 2 },
      status: "critical",
      pages: { critical: 1, warning: 1, info: 0 },
      workloads: [
        { name: "checkout", kind: "Deployment", ready: "1/3", status: "critical", note: "readiness probe failures" },
        { name: "report-worker", kind: "Deployment", ready: "2/2", status: "degraded", note: "restarting" },
        { name: "cart", kind: "Deployment", ready: "3/3", status: "healthy" },
      ],
    },
    {
      name: "payments",
      pods: { ready: 18, total: 20 },
      counts: { deployments: 3, statefulSets: 1, cronJobs: 0, persistentVolumeClaims: 3 },
      status: "degraded",
      pages: { critical: 0, warning: 1, info: 1 },
      workloads: [
        { name: "payments-api", kind: "Deployment", ready: "2/3", status: "degraded", note: "OOMKilled" },
        { name: "search-indexer", kind: "StatefulSet", ready: "3/3", status: "healthy" },
      ],
    },
    {
      name: "platform",
      pods: { ready: 18, total: 16 },
      counts: { deployments: 2, statefulSets: 0, cronJobs: 3, persistentVolumeClaims: 1 },
      status: "healthy",
      pages: { critical: 0, warning: 0, info: 1 },
      workloads: [{ name: "image-sync", kind: "CronJob", ready: "1/1", status: "healthy" }],
    },
  ],
  nodesDetail: [
    { name: "ip-10-0-1-17", zone: "eu-west-1a", status: "ready", cpuPercent: 41, memoryPercent: 62, pods: 31, conditions: [] },
    {
      name: "ip-10-0-2-21",
      zone: "eu-west-1b",
      status: "pressure",
      cpuPercent: 93,
      memoryPercent: 94,
      pods: 27,
      conditions: ["MemoryPressure"],
    },
  ],
};

const settings = {
  managedBy: "portal",
  writeCredentials: true,
  version: "v2.0.0",
  ai: {
    baseUrl: "http://ollama.monitoring.svc:11434/v1",
    model: "qwen3-coder-30b",
    apiKey: { configured: true, fingerprint: "sha256:8f2c1ad4", updatedAt: iso(259_200_000) },
  },
  repository: {
    owner: "a-hagi613",
    repo: "pagerless-sandbox",
    branch: "main",
    autoDraftPR: false,
    token: { configured: true, fingerprint: "sha256:44aa91f0", updatedAt: iso(86_400_000) },
  },
  connectors: [
    { name: "native_k8s_logs", type: "logs", enabled: true, url: "", token: { configured: false }, locked: true, status: "ok" },
    { name: "loki", type: "logs", enabled: false, url: "", token: { configured: false }, status: "unconfigured" },
    {
      name: "prometheus",
      type: "metrics",
      enabled: true,
      url: "http://prometheus.monitoring.svc:9090",
      token: { configured: true, fingerprint: "sha256:1b7e0c42" },
      status: "ok",
    },
    { name: "cloudwatch", type: "logs", enabled: false, url: "", token: { configured: false }, status: "unconfigured" },
    { name: "datadog", type: "metrics", enabled: false, url: "", token: { configured: false }, status: "unconfigured" },
  ],
  alertmanagerWebhookUrl: "http://pagerless.pagerless.svc:8080/v1/webhooks/alerts",
};

const state = { authenticated: false, managedBy: "portal", pages: structuredClone(pages), settings: structuredClone(settings) };

function json(res, status, body) {
  const payload = JSON.stringify(body);
  res.writeHead(status, { "content-type": "application/json", "content-length": Buffer.byteLength(payload) });
  res.end(payload);
}

function readBody(req) {
  return new Promise((resolve) => {
    let raw = "";
    req.on("data", (chunk) => {
      raw += chunk;
    });
    req.on("end", () => {
      try {
        resolve(raw === "" ? {} : JSON.parse(raw));
      } catch {
        resolve({});
      }
    });
  });
}

function detailOf(page) {
  return {
    ...page,
    findings: page.findings ?? [],
    steps: page.steps ?? [],
  };
}

function summaryOf(page) {
  const { diagnosis, findings, steps, proposal, usage, narrative, ...summary } = page;
  void diagnosis;
  void findings;
  void steps;
  void proposal;
  void usage;
  void narrative;
  return summary;
}

function listResponse() {
  return { pages: state.pages.map(summaryOf), matrix };
}

function authorized(req) {
  if (process.env.MOCK_AUTO_AUTH === "1") {
    // Accessibility scans load the Portal without a browser session; this flag exists only for those
    // local runs and is never set by the Playwright suite, which signs in through the real form.
    return true;
  }
  return state.authenticated && (req.headers.cookie ?? "").includes(sessionCookie);
}

function requireSession(req, res) {
  if (!authorized(req)) {
    json(res, 401, { error: { code: "unauthenticated", message: "sign in first" } });
    return false;
  }
  return true;
}

function requireCSRF(req, res) {
  if (req.headers["x-csrf-token"] !== csrfToken) {
    json(res, 403, { error: { code: "csrf", message: "missing or wrong X-CSRF-Token" } });
    return false;
  }
  return true;
}

const server = createServer((req, res) => {
  const url = new URL(req.url ?? "/", `http://127.0.0.1:${String(port)}`);
  const path = url.pathname;
  const method = req.method ?? "GET";

  if (path === "/api/auth/session" && method === "GET") {
    if (!authorized(req)) {
      json(res, 401, { error: { code: "unauthenticated", message: "no session" } });
      return;
    }
    json(res, 200, {
      authenticated: true,
      username: "admin",
      mode: "token",
      canApprove: true,
      csrfToken,
      expiresAt: iso(-28_800_000),
    });
    return;
  }

  if (path === "/api/auth/login" && method === "POST") {
    void readBody(req).then((body) => {
      if (body.token !== "test-bootstrap-token") {
        json(res, 401, { error: { code: "invalid_token", message: "invalid token" } });
        return;
      }
      state.authenticated = true;
      res.writeHead(200, {
        "content-type": "application/json",
        "set-cookie": `${sessionCookie}; Path=/; HttpOnly; SameSite=Lax`,
      });
      res.end(JSON.stringify({ expiresAt: iso(-28_800_000) }));
    });
    return;
  }

  if (path === "/api/auth/logout" && method === "POST") {
    if (!requireSession(req, res) || !requireCSRF(req, res)) {
      return;
    }
    state.authenticated = false;
    res.writeHead(204, { "set-cookie": "pagerless_session=; Path=/; Max-Age=0" });
    res.end();
    return;
  }

  // Test-only controls: reset the fixtures between tests, and flip credential ownership so the
  // read-only state is exercised. Neither route exists in pagerless-server.
  if (path === "/__test/reset" && method === "POST") {
    state.authenticated = false;
    state.managedBy = "portal";
    state.pages = structuredClone(pages);
    state.settings = structuredClone(settings);
    json(res, 200, { reset: true });
    return;
  }

  if (path === "/__test/ownership" && method === "POST") {
    void readBody(req).then((body) => {
      state.managedBy = body.managedBy === "external" ? "external" : "portal";
      state.settings.managedBy = state.managedBy;
      state.settings.writeCredentials = state.managedBy === "portal";
      json(res, 200, { managedBy: state.managedBy });
    });
    return;
  }

  if (!path.startsWith("/api/")) {
    json(res, 404, { error: { code: "not_found", message: `no route ${method} ${path}` } });
    return;
  }

  if (!requireSession(req, res)) {
    return;
  }

  if (path === "/api/pages" && method === "GET") {
    json(res, 200, listResponse());
    return;
  }

  if (path === "/api/pages" && method === "POST") {
    if (!requireCSRF(req, res)) {
      return;
    }
    void readBody(req).then((body) => {
      const name = `${String(body.name ?? "workload").toLowerCase()}-new1`;
      const created = {
        name,
        phase: "Investigating",
        severity: "warning",
        subject: {
          apiVersion: "apps/v1",
          kind: body.kind ?? "Deployment",
          namespace: body.kind === "Node" ? undefined : (body.namespace ?? "shop"),
          name: String(body.name ?? "workload"),
          uid: `uid-${name}`,
          resolution: "direct",
        },
        createdAt: new Date().toISOString(),
        triggerCount: 1,
        title: `Investigating ${String(body.name ?? "workload")}`,
        signal: "opened by hand · investigation started",
        diagnosis: undefined,
        findings: [],
        steps: [],
      };
      state.pages = [created, ...state.pages];
      json(res, 201, detailOf(created));
    });
    return;
  }

  const pageMatch = /^\/api\/pages\/([^/]+)(\/trace|\/decision)?$/.exec(path);
  if (pageMatch !== null) {
    const name = decodeURIComponent(pageMatch[1] ?? "");
    const suffix = pageMatch[2];
    const page = state.pages.find((candidate) => candidate.name === name);
    if (page === undefined) {
      json(res, 404, { error: { code: "not_found", message: `no Page ${name}` } });
      return;
    }

    if (suffix === undefined && method === "GET") {
      json(res, 200, detailOf(page));
      return;
    }

    if (suffix === "/trace" && method === "GET") {
      res.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
        "x-accel-buffering": "no",
      });
      const send = (event, data) => {
        res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
      };
      for (const step of page.steps ?? []) {
        send("step", step);
      }
      if (page.phase !== "Investigating") {
        send("done", { phase: page.phase });
        res.end();
        return;
      }
      const timers = [];
      liveSteps.forEach((step, index) => {
        timers.push(
          setTimeout(() => {
            page.steps = [...(page.steps ?? []), step];
            send("step", step);
          }, 400 * (index + 1)),
        );
      });
      timers.push(
        setTimeout(() => {
          page.phase = "Proposed";
          page.proposal = {
            ...proposal,
            summary: "Raise the probe timeout to 4s",
            hash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            branch: `pagerless/${page.name}-aaaaaaaa`,
          };
          page.proposalHash = page.proposal.hash;
          page.diagnosis = "The probe timeout is shorter than the measured cold-start latency.";
          page.findings = [{ id: "f-live", kind: "Metric", source: "Prometheus", summary: "cold start p99 is 2.41s" }];
          send("phase", { phase: page.phase });
        }, 1_400),
      );
      timers.push(
        setTimeout(() => {
          send("done", { phase: page.phase });
          res.end();
        }, 1_800),
      );
      req.on("close", () => {
        for (const timer of timers) {
          clearTimeout(timer);
        }
      });
      return;
    }

    if (suffix === "/decision" && method === "POST") {
      if (!requireCSRF(req, res)) {
        return;
      }
      void readBody(req).then((body) => {
        if (body.proposalHash !== page.proposalHash) {
          json(res, 409, { error: { code: "stale", message: "the Proposal changed; review the new hash" } });
          return;
        }
        if (body.decision === "approve") {
          page.phase = "PullRequestOpen";
          page.pullRequest = {
            number: 419,
            url: "https://github.com/a-hagi613/pagerless-sandbox/pull/419",
            state: "draft",
          };
        } else {
          page.phase = "Rejected";
        }
        page.decision = {
          outcome: body.decision,
          decidedBy: "admin",
          decidedAt: new Date().toISOString(),
          proposalHash: body.proposalHash,
          reason: body.reason ?? "",
        };
        json(res, 200, detailOf(page));
      });
      return;
    }
    json(res, 405, { error: { code: "method_not_allowed", message: `${method} ${path}` } });
    return;
  }

  if (path === "/api/settings" && method === "GET") {
    json(res, 200, state.settings);
    return;
  }

  if (path === "/api/settings" && method === "PATCH") {
    if (!requireCSRF(req, res)) {
      return;
    }
    void readBody(req).then((body) => {
      if (state.settings.managedBy === "external" || state.settings.writeCredentials === false) {
        json(res, 409, {
          error: {
            code: "credential_writes_disabled",
            message: "credentials are managed by an external secret manager",
          },
        });
        return;
      }
      if (body.ai !== undefined) {
        state.settings.ai = {
          ...state.settings.ai,
          baseUrl: body.ai.baseUrl ?? state.settings.ai.baseUrl,
          model: body.ai.model ?? state.settings.ai.model,
          apiKey:
            body.ai.apiKey === undefined
              ? state.settings.ai.apiKey
              : body.ai.apiKey === ""
                ? { configured: false }
                : { configured: true, fingerprint: "sha256:newfingerprint", updatedAt: new Date().toISOString() },
        };
      }
      if (body.repository !== undefined) {
        state.settings.repository = {
          ...state.settings.repository,
          owner: body.repository.owner ?? state.settings.repository.owner,
          repo: body.repository.repo ?? state.settings.repository.repo,
          branch: body.repository.branch ?? state.settings.repository.branch,
          autoDraftPR: body.repository.autoDraftPR ?? state.settings.repository.autoDraftPR,
          token:
            body.repository.token === undefined
              ? state.settings.repository.token
              : body.repository.token === ""
                ? { configured: false }
                : { configured: true, fingerprint: "sha256:newtokenfing" },
        };
      }
      for (const patch of body.connectors ?? []) {
        state.settings.connectors = state.settings.connectors.map((connector) =>
          connector.name === patch.name
            ? {
                ...connector,
                enabled: patch.enabled ?? connector.enabled,
                url: patch.url ?? connector.url,
                token:
                  patch.token === undefined || patch.token === ""
                    ? connector.token
                    : { configured: true, fingerprint: "sha256:newconnectort" },
                status: patch.enabled === false ? "unconfigured" : "ok",
              }
            : connector,
        );
      }
      json(res, 200, state.settings);
    });
    return;
  }

  if (path === "/api/settings/test" && method === "POST") {
    if (!requireCSRF(req, res)) {
      return;
    }
    void readBody(req).then((body) => {
      const target = String(body.target ?? "");
      if (target.includes("datadog")) {
        json(res, 502, { error: { code: "upstream_failed", message: "datadog returned 403 for the configured token" } });
        return;
      }
      json(res, 200, { ok: true, detail: `${target} answered in 214ms` });
    });
    return;
  }

  json(res, 404, { error: { code: "not_found", message: `no route ${method} ${path}` } });
});

server.listen(port, "127.0.0.1", () => {
  process.stdout.write(`mock api listening on http://127.0.0.1:${String(port)}\n`);
});
