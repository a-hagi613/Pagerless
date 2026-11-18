# Research: cluster failure watching, read-only log adapters, GitOps file mapping, and `kubectl page` distribution

Resolution of [Research Kubernetes failure watching, pluggable log APIs, GitOps file mapping, and Krew distribution](https://github.com/a-hagi613/pagerless/issues/78), a child ticket of the `Pagerless v2.0` map.

Every technical claim below is tied to the primary source that owns it: upstream Go source, official API references, vendor HTTP API documentation, or a file in this repository with line numbers. All URLs were fetched and checked on 2026-11-18; nothing here is quoted from memory. Where the ticket's framing conflicts with the sources, the correction is called out explicitly (the most consequential one is in [§1.1](#11-two-event-apis-and-one-factual-correction)).

This document is a decision input, not an implementation. Code shapes shown here are proposals derived from the cited facts and are marked as such.

---

## Summary of findings

| Question | Answer in one line |
| --- | --- |
| Q1 watching | Cluster-wide read-only watching is a `ClusterRole` with `get`/`list`/`watch`, but the durable signal is not the Event object: `events.k8s.io/v1` replaces `involvedObject` with `regarding`, the API server only retains Events for one hour by default, and `CrashLoopBackOff` is a display string rather than a phase, so a watcher needs both Events and `.status` conditions plus its own persistence. |
| Q1 deduplication | Upstream `client-go` already defines the canonical aggregate key (source, involved object, type, reason, reporting controller/instance — the message is deliberately excluded) and default aggregation/spam-filter constants (10 events per 600 s; burst 25, refill 1 per 300 s); reuse those shapes and persist the fingerprint and cooldown on the `Page` rather than in memory. |
| Q2 adapters | All five external backends expose read-only query endpoints with simple bearer/basic/API-key headers except AWS, which requires SigV4 and an `X-Amz-Target` action; a normalized `AlertPayload` can be derived from four webhook dialects whose field names are documented, and only SNS carries a signature to verify. |
| Q2 MCP | MCP's `stdio` and Streamable HTTP transports are both standardized, but an external MCP server adds a process hop per query and cannot be versioned with the chart's values; a built-in adapter interface with an optional MCP bridge is the smaller surface for a Helm-only install. |
| Q3 mapping | Argo CD's *default* tracking method is the `argocd.argoproj.io/tracking-id` annotation, not a label; Flux stamps `<group>/name` and `<group>/namespace` labels via `fluxcd/pkg/ssa`; Helm stamps `meta.helm.sh/release-name`/`release-namespace` annotations plus `app.kubernetes.io/managed-by=Helm`. Fallbacks (recursive Trees, Code Search, Contents) have hard limits that change the algorithm. |
| Q3 permissions | `Contents: write` (create refs, create/update files), `Pull requests: write` (open the PR), `Metadata: read` — nothing else, and *no* cluster write verb is needed if validation happens in the pull request's CI rather than through a server-side dry run. |
| Q4 packaging | `kubectl-page` is published as Krew plugin `page` (`plugins/page.yaml`), manifest `apiVersion: krew.googlecontainertools.github.com/v1alpha2`; GoReleaser produces the archives, the SHA-256 checksum file, and can drive both the Krew manifest and a Homebrew cask. |

---

## 1. Cluster-wide Kubernetes failure watching and deduplication

### 1.1 Two Event APIs, and one factual correction

Kubernetes serves two Event representations. They are not aliases, and the field the ticket names (`involvedObject`) exists in only one of them.

**`core/v1` Event** (`k8s.io/api/core/v1`, the original, still served at `/api/v1/events`):

| Field | Meaning |
| --- | --- |
| `involvedObject` | the object the event is about |
| `reason` | short machine-readable cause |
| `message` | human-readable description |
| `source` | reporting component |
| `firstTimestamp` / `lastTimestamp` / `count` | repetition bookkeeping |
| `type` | `Normal` or `Warning` |
| `eventTime`, `series`, `action` | newer additions, carried alongside the legacy fields |

Source: [`kubernetes/api/core/v1/types.go`, `type Event` at line 8246, `InvolvedObject` 8253, `Source` 8268, `FirstTimestamp`/`LastTimestamp`/`Count` 8272–8280, `EventTime`/`Series`/`Action` 8288–8296](https://github.com/kubernetes/api/blob/master/core/v1/types.go).

**`events.k8s.io/v1` Event** (the dedicated events API, marked `+k8s:prerelease-lifecycle-gen:introduced=1.19`):

| Field | Meaning |
| --- | --- |
| `eventTime` | required; when the event was first observed (microsecond precision) |
| `series` | `{count, lastObservedTime}` when the event repeats, nil for a singleton |
| `reportingController` | required; e.g. `kubernetes.io/kubelet` |
| `reportingInstance` | required; the controller instance, e.g. `kubelet-xyzf` |
| `action` | required; what was done, machine-readable |
| `reason` | required; why, human-readable |
| `regarding` | the object the event is about |
| `related` | optional secondary object |
| `note` | description, "maximal length … 1kB, but libraries should be prepared to handle values up to 64kB" |
| `type` | `Normal`, `Warning` |
| `deprecatedSource`, `deprecatedFirstTimestamp`, `deprecatedCount`, … | compatibility carry-overs from `core/v1` |

Source: [`kubernetes/api/events/v1/types.go`](https://github.com/kubernetes/api/blob/master/events/v1/types.go) — `introduced=1.19` at lines 26 and 122, `Event` 34, `EventTime` 44, `Series` 48, `ReportingController` 53, `ReportingInstance` 58, `Action` 63, `Reason` 68, `Regarding` 74, `Related` 79, `Note` 85, `Type` 91, deprecated fields 93–104, `EventSeries{Count, LastObservedTime}` 112–119.

The same fields are enumerated in the generated API reference: [`kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#event-v1-events-k8s-io`](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#event-v1-events-k8s-io) (contains `regarding`, `reportingController`, `reportingInstance`, `series`, `note`) and [`#event-v1-core`](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#event-v1-core) (contains `involvedObject`, `reportingComponent`, `source`, `count`, `firstTimestamp`, `lastTimestamp`). Note that `kubernetes.io/docs/reference/kubernetes-api/cluster-resources/event-v1/` renders the **core/v1** field set — its table is `involvedObject`, `reportingComponent`, `source`, `count`, `firstTimestamp`, `lastTimestamp` — so it is not the page to cite for the new API.

**Correction to the ticket:** `events.k8s.io/v1` has no `involvedObject`; the equivalent is `regarding`. A watcher that binds `events.k8s.io/v1` and reads `involvedObject` decodes to an empty `ObjectReference` and silently matches nothing. Pagerless should bind **both** APIs, or bind `events.k8s.io/v1` and read `regarding`, treating `core/v1` as the legacy fallback for older or third-party emitters.

### 1.2 What "failing" looks like, beyond Events

Events alone are lossy: they expire, they can be dropped, and the most familiar failure string is not part of the object model.

- **`CrashLoopBackOff` is a `kubectl` display field, not a Pod phase.** The lifecycle documentation says so directly: "When a pod is failing to start repeatedly, `CrashLoopBackOff` may appear in the Status field of some `kubectl` commands … Make sure not to confuse Status, a `kubectl` display field for user intuition, with the pod's phase." Source: [Pod Lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/).
- Kubernetes restarts crashing containers with **exponential backoff**, which is why an image-pull or crash storm produces a long tail of identical signals rather than one: "After the initial crash Kubernetes applies an exponential backoff delay for subsequent restarts … If a container runs successfully for a certain duration (e.g., 10 minutes), Kubernetes resets the backoff delay." Source: [Pod Lifecycle, "How Pods handle problems with containers"](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/).
- The structured signals a watcher should read instead of strings:

| Resource | Durable failure signal | Source |
| --- | --- | --- |
| Pod | `status.conditions[]` types `Initialized`, `Ready`, `PodScheduled`; `status.containerStatuses[].state.waiting.reason`; `restartCount` | [`core/v1/types.go`: `PodInitialized` 3868, `PodReady` 3871, `PodScheduled` 3873, `PodReasonUnschedulable` 3904, `ContainerStateWaiting.Reason` 3602–3608, `ContainerStatus.RestartCount` 3683](https://github.com/kubernetes/api/blob/master/core/v1/types.go) |
| Deployment | `status.conditions[]` `Available`, `Progressing`, `ReplicaFailure` | [`apps/v1/types.go` 665–673](https://github.com/kubernetes/api/blob/master/apps/v1/types.go) |
| Job | `status.conditions[]` `Complete`, `Failed`, `Suspended` | [`batch/v1/types.go` 722–727](https://github.com/kubernetes/api/blob/master/batch/v1/types.go) |
| Node | `status.conditions[]` `Ready`, `MemoryPressure`, `DiskPressure`, `PIDPressure` | [`core/v1/types.go` 7543–7549](https://github.com/kubernetes/api/blob/master/core/v1/types.go) |
| PersistentVolumeClaim | `status.phase`: `Pending`, `Bound`, `Lost` (no conditions) | [`core/v1/types.go` 1040–1046](https://github.com/kubernetes/api/blob/master/core/v1/types.go) |

Consequence for the design: the watcher must be **two-sourced**. Events explain *why* (image pull failure, failed mount, back-off), status explains *that it is still true*. A Page opened purely from an Event can outlive the condition; a Page opened purely from status cannot say why.

### 1.3 RBAC: the exact read-only cluster-wide grant

Kubernetes RBAC grants access per API group/resource/verb, and cluster scope is expressed with a `ClusterRole` plus `ClusterRoleBinding`; a namespaced `Role` cannot grant a cluster-wide watch. Subresources are addressed with a slash: the reference's own example grants `resources: ["pods", "pods/log"]` with `verbs: ["get", "list"]`, which shows that `pods/log` is a separate resource entry, not implied by `pods`. Source: [Using RBAC Authorization](https://kubernetes.io/docs/reference/access-authn-authz/rbac/).

A least-privilege grant for the resource list in the ticket, excluding Secrets, `exec`, `port-forward`, and every write verb:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: pagerless-watcher
rules:
  - apiGroups: [""]
    resources: [pods, services, configmaps, persistentvolumeclaims, events]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]                     # subresource: no list/watch, no exec
  - apiGroups: [events.k8s.io]
    resources: [events]
    verbs: [get, list, watch]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets, replicasets]
    verbs: [get, list, watch]
  - apiGroups: [batch]
    resources: [jobs, cronjobs]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [nodes]
    verbs: [get, list, watch]        # cluster-scoped; no watch of nodes/status needed
  - apiGroups: [networking.k8s.io]
    resources: [ingresses]
    verbs: [get, list, watch]
```

Notes that matter for review:

- **No `secrets`, no `pods/exec`, no `pods/portforward`, no `create`/`update`/`patch`/`delete` anywhere.** `pods/exec` and `pods/portforward` are separate subresources with their own verbs, so omitting them is sufficient — the RBAC reference documents that subresources are named `resource/subresource` and granted independently.
- **`events` exists in two groups** (`""` and `events.k8s.io`), so both rules are required for a watcher that consumes both APIs.
- The **`resourceNames` field cannot narrow `list`/`watch`** in a useful way here: it restricts requests to named instances, and a watch across a namespace set would have to enumerate names. The RBAC reference documents `resourceNames` as "requests can be restricted to individual instances of a resource" (example: a single ConfigMap), which is the wrong shape for a cluster watcher. Source: [Using RBAC Authorization, "Referring to resources"](https://kubernetes.io/docs/reference/access-authn-authz/rbac/).
- **Field selectors can narrow the watch server-side**, which reduces cache size without widening permission: Event supports `involvedObject.kind`, `involvedObject.namespace`, `involvedObject.name`, `involvedObject.uid`, `involvedObject.apiVersion`, `involvedObject.resourceVersion`, `involvedObject.fieldPath`, `reason`, `reportingComponent`, `source`, `type`. Source: [Field Selectors](https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors/).

**The existing chart already violates the strict read-only invariant in one place.** `charts/pagerless/templates/rbac.yaml` grants, per workload namespace:

- `deployments, statefulsets` with `verbs: [get, update]` (lines 198–204) — an `update` verb, described in the comment as "Admission independently rejects live writes, even when the server can authorize an update for a dry run";
- `pods` with `get, list, watch, delete` for the Executor (lines 236–241), the v1 `RestartWorkload` path;
- `nodes` with `get, list` cluster-wide (lines 107–109).

The `update` grant exists because `internal/patchresources/validator.go:184` performs `r.writer.Update(ctx, live.Object, client.DryRunAll)`. That is the mechanism that must be re-scoped for v2: **server-side dry run is an authorized write**. The API server runs the full request pipeline, including authorization, and only skips persistence — "When you set `?dryRun=All`, any relevant admission controllers are run, validating admission controllers check the request post-mutation, merge is performed on PATCH, fields are defaulted, and schema validation occurs. The changes are not persisted to the underlying storage". Source: [Kubernetes API Concepts, "Dry-run"](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run). There is no read-only way to server-dry-run a Deployment patch.

### 1.4 Deterministic fingerprinting

The upstream precedent is `client-go`'s event correlator, which already had to answer "are these two Events the same problem?".

- The aggregate key is composed of `Source.Component`, `Source.Host`, `InvolvedObject.Kind`, `InvolvedObject.Namespace`, `InvolvedObject.Name`, `InvolvedObject.UID`, `InvolvedObject.APIVersion`, `Type`, `Reason`, `ReportingController`, `ReportingInstance`; the **message is the `localKey`, not part of the aggregate key**. Source: [`kubernetes/client-go/tools/record/events_cache.go`, `EventAggregatorByReasonFunc` at lines 160–177, doc comment at 154–162](https://github.com/kubernetes/client-go/blob/master/tools/record/events_cache.go).
- The same file fixes the default noise constants: `defaultAggregateMaxEvents = 10` and `defaultAggregateIntervalInSeconds = 600` ("if we see the same event that varies only by message more than 10 times in a 10 minute period, aggregate the event"), and the spam filter's `defaultSpamBurst = 25` with `defaultSpamQPS = 1. / 300.` ("allow a source to send 25 events about an object but control the refill rate to 1 new event every 5 minutes", aimed at "the long-tail of events for things that are always unhealthy"). Source: [same file, lines 34–50](https://github.com/kubernetes/client-go/blob/master/tools/record/events_cache.go).

Design consequence: **never hash the message/note.** Kubernetes notes embed volatile text (back-off milliseconds, image digests, timestamps). A fingerprint over the note turns one crash loop into hundreds of identities.

Recommended fingerprint input (proposal, derived from the fields cited above):

```
fingerprint = SHA-256(
    "pagerless.dev/page-fingerprint/v1\n" ||
    clusterID || "\n" ||
    apiGroup || "/" || kind || "\n" ||
    namespace || "\n" ||
    name || "\n" ||
    uid || "\n" ||               // survives rename, dies with recreation
    reason || "\n" ||            // e.g. CrashLoopBackOff, FailedScheduling
    container || "\n"            // empty for non-Pod kinds
)
```

This mirrors the domain-separated hashing the repository already uses for Proposals: `internal/proposal/hash.go:21` defines `hashDomain = "pagerless.dev/proposal/v1\n"` and `hash.go:106–111` encodes a fixed-order struct with `encoding/json` (map fields converted to sorted slices) before `sha256.Sum256(append([]byte(hashDomain), encoded...))`. Reusing that pattern keeps one reviewable hashing convention in the codebase instead of two. The repo's comment at `hash.go:23–27` states the reason the encoding is canonical: "equal Proposals encode to identical bytes".

Two details worth locking:

- **Include `uid`, not just `name`.** A recreated Deployment with the same name is a different failure subject; without the UID a new crash loop joins the old Page.
- **Exclude `count`/`series.count`.** Repetition is the thing being suppressed; putting it in the key defeats deduplication.

### 1.5 Cooldown, aggregation, and Active-Page suppression

Three independent mechanisms are needed, because each covers a failure mode the others do not:

In-process coalescing. `client-go`'s workqueue already deduplicates by key: an item is added to a `dirty` set and only enqueued if it is not already in `processing`, so N rapid changes to one object collapse into one pending reconcile. Source: [`client-go/util/workqueue/queue.go`, `dirty`/`processing` sets at lines 184–221, `Add` at 243–266](https://github.com/kubernetes/client-go/blob/master/util/workqueue/queue.go). Controller-runtime's controller consumes exactly this abstraction (`NewQueue func(...) workqueue.TypedRateLimitingInterface[request]` in the controller options), so nothing extra is needed to stop a storm from spawning N goroutines; something is still needed to stop it spawning N Pages. Source: [`controller-runtime/pkg/internal/controller/controller.go`, lines 58 and 549–556](https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/internal/controller/controller.go).

**(b) Watch-level filtering.** Controller-runtime predicates can drop updates that cannot change the answer: `GenerationChangedPredicate` (spec changes bump `metadata.generation`), `ResourceVersionChangedPredicate`, `AnnotationChangedPredicate`, `LabelChangedPredicate`, and `Funcs` for arbitrary predicates. Source: [`controller-runtime/pkg/predicate/predicate.go`, lines 55–56, 153–175, 229–250, 262–274](https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/predicate/predicate.go). Wiring is `WithEventFilter`/`Watches` on the builder: [`pkg/builder/controller.go`, `Watches` 159, `WithEventFilter` 230](https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/builder/controller.go).

Caveat that decides the predicate choice: **status-only changes do not bump `generation`**, so `GenerationChangedPredicate` would suppress exactly the `.status.conditions` transitions §1.2 depends on. Use a custom predicate on the specific condition, not the generation predicate.

**(c) Durable cooldown and Active-Page suppression, stored on the Page.** Events are short-lived: the API server retains them for `--event-ttl`, whose default is `1h0m0s`. Source: [kube-apiserver command-line reference, `--event-ttl`](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-apiserver/). Therefore:

- A cooldown longer than one hour **cannot** be derived from the event stream; the evidence is gone. Persist `lastPagedAt` / `suppressedCount` in the `Page` status.
- The suppression rule is "one **active** Page per fingerprint". The repository already has the canonicalisation primitive for this problem: `internal/triage/active.go:29–31` (`CanonicalActive`) lists all Triages in the namespace, filters by workload and active phase, sorts by `CreationTimestamp`, then `Name`, then `UID` for a stable winner (`active.go:48–56`), and returns the rest as duplicates. The Page watcher should reuse this exact shape: select, don't race. The alternative — create-and-expect-a-webhook-to-reject — is what `internal/alerts/receiver.go:153–175` does today (`trigger` retries `join` up to `maxAttempts = 3` around the duplicate-refusal webhook), which is correct but only because the webhook is authoritative.
- Backoff state must be **reset on recovery**, mirroring kubelet: a container that runs 10 minutes resets its backoff ([Pod Lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)). Otherwise a workload that flaps once a week accumulates a permanently long cooldown.

Recommended constants, derived from the upstream figures rather than invented: aggregate identical fingerprints inside a **600 s** window; allow at most **10** occurrences to escalate severity; apply an **event-driven burst ceiling** of 25 per object with a 1-per-300 s refill for anything that only opens a Page when the condition persists. Those are the `client-go` defaults in the citation above, so operators can reason about Pagerless with the same intuition they use for `kubectl` events.

### 1.6 Where this lands in the existing code

- The repository today starts Triages from **webhooks only**: `internal/alerts/receiver.go:27` (`Path = "/v1/alerts/alertmanager"`), mounted at `internal/server/server.go:469`. There is no cluster-wide Event watcher; the bundled read role exists solely for the agent's MCP evidence bundle (`charts/pagerless/templates/rbac.yaml:258–285`, `pods, events` `get/list/watch`, `pods/log` `get`, `events.k8s.io` `events` `get/list/watch`).
- The only controllers are `TriageReconciler` (`internal/controller/triage_controller.go:47–56`, `For(&v1alpha1.Triage{})`, `Owns(&batchv1.Job{})`) and the verification reconciler (`internal/controller/verification.go:59`). Neither watches workloads.
- The `alerts.Receiver` is Alertmanager-shaped: `payload.Alerts[].Status/Labels/Fingerprint` (`receiver.go:66–76`), opt-in via the `pagerless_triage=true` label (`receiver.go:31`), namespace allowlist (`receiver.go:134–137`), bearer-token auth with constant-time compare (`receiver.go:98–104`), and a 1 MiB body cap (`receiver.go:41`). Any new watcher should feed the same Triage/Trigger path rather than creating a second write path.

**Gap to decide:** an Event-driven Page and a webhook-driven Triage must collapse into the same Active object for a fingerprint, or the same incident will be opened twice. The cheapest correct option is to make the fingerprint the join key and let `CanonicalActive`'s select-then-sort logic decide the winner.

---

## 2. Read-only log/metrics adapters and inbound alert webhooks

### 2.1 Adapter interface

The invariant is that an adapter can only *read*: it issues one query, returns a bounded page, and has no verb that could mutate the backend. Proposal:

```go
type Query struct {
    Namespace string
    Workload  v1alpha1.WorkloadReference
    Pod       string    // optional
    Container string    // optional
    Since     time.Time
    Until     time.Time
    Limit     int       // hard page cap, enforced by the caller
    Filter    string    // adapter-specific, passed through verbatim
}

type Entry struct {
    Timestamp time.Time
    Stream    map[string]string // Loki/OpenSearch labels, CloudWatch stream, Prometheus metric
    Line      string
}

type Page struct {
    Entries   []Entry
    NextToken string // opaque cursor: nextToken, page.after, from
    Truncated bool
}

type Adapter interface {
    Name() string
    Query(ctx context.Context, q Query) (Page, error) // one bounded read, no retries with writes
}
```

Rules that follow from the sources below: a deadline on every call; a byte budget enforced by the caller (the existing receiver caps request bodies at 1 MiB, `internal/alerts/receiver.go:41`); pagination is explicit (`nextToken`, `page.after`) rather than "fetch everything"; and authentication is per-adapter configuration read from a Secret, never defaulted.

### 2.2 Read-only query endpoints

| Backend | Request | Auth | Bounded by | Response shape |
| --- | --- | --- | --- | --- |
| Kubernetes | `GET /api/v1/namespaces/{namespace}/pods/{name}/log` | kubeconfig / ServiceAccount token | `limitBytes`, `tailLines`, `sinceSeconds` | plain text stream |
| Prometheus | `GET` or `POST /api/v1/query`, `/api/v1/query_range` | HTTP basic auth against the API, or a bearer token configured by the operator | `limit`, `step`, `timeout` | `{"status":"success","data":{"resultType":"matrix\|vector\|scalar\|string","result":…}}` |
| Grafana Loki | `GET /loki/api/v1/query_range` | HTTP basic auth, or `X-Scope-OrgID` for multi-tenant | `limit`, `since`, `step`, `direction` | `{"status":"success","data":{"resultType":"streams","result":[{"stream":{…},"values":[[ns,"line"],…]}]}}` |
| AWS CloudWatch Logs | `POST /` with `X-Amz-Target: Logs_20140328.FilterLogEvents` (or `StartQuery` + `GetQueryResults`) | SigV4 (`Authorization: AWS4-HMAC-SHA256 …`) | `limit` (page: ≤1 MB or ≤10,000 events), `startTime`/`endTime` ms | `{"events":[{"timestamp","message","logStreamName","eventId","ingestionTime"}],"nextToken":…}` |
| Datadog | `POST /api/v2/logs/events/search` | `DD-API-KEY` + `DD-APPLICATION-KEY` headers (or OAuth `AuthZ`) | `page.limit`, `filter.from`/`filter.to` | `{"data":[…],"meta":{"page":{"after":…}}}` |
| OpenSearch | `GET`/`POST /{index}/_search` | `Authorization: ApiKey os_…` (Security plugin), or basic auth | `size`, `from`, `search_after` | standard `hits.hits[]` |

Detailed citations:

**Kubernetes `pods/log`.** `GET /api/v1/namespaces/{namespace}/pods/{name}/log` with query parameters `container` ("Defaults to only container if there is one container in the pod"), `follow` (default false), `previous` ("Return previous terminated container logs. Defaults to false"), `sinceSeconds`, `tailLines`, `limitBytes`, `timestamps`. Source: [Kubernetes API reference, "Read Log", `#read-log-pod-v1-core`](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#read-log-pod-v1-core). `previous: true` is what makes a crash loop debuggable after the restart, and it is why the read RBAC needs `pods/log` `get` and nothing more.

**Prometheus.** `GET /api/v1/query` takes `query`, `time`, `timeout`, `limit`, `lookback_delta`; `GET /api/v1/query_range` takes `query`, `start`, `end`, `step`, `timeout`, `limit`, `lookback_delta`, `stats`. Large queries can be sent as a form body: "You can URL-encode these parameters directly in the request body by using the `POST` method and `Content-Type: application/x-www-form-urlencoded` header." The data section is `{"resultType": "matrix" | "vector" | "scalar" | "string", "result": <value>}`. Source: [Prometheus HTTP API](https://prometheus.io/docs/prometheus/latest/querying/api/). Authentication to the HTTP API is basic auth configured by the operator (`--web.config.file`), documented in [Securing Prometheus API and UI endpoints using basic auth](https://prometheus.io/docs/guides/basic-auth/); the adapter should therefore accept a username/password pair *or* an `Authorization` header value, and send the latter verbatim.

**Grafana Loki.** `GET /loki/api/v1/query_range` accepts `query` (LogQL), `limit` ("defaults to 100"), `start`/`end`, `step`, `interval`, `direction` (`forward|backward`, default `backward`). The response is `{"status":"success","data":{"resultType":"vector"|"streams","result":[…]}}`, with stream results carrying `"values": [["<nanosecond unix epoch>", "<log line>"], …]`. Multi-tenancy is selected with `X-Scope-OrgID`, and the docs' own example uses basic auth (`curl -u "User:$API_TOKEN"`). Source: [Loki HTTP API](https://grafana.com/docs/loki/latest/reference/loki-http-api/).

**AWS CloudWatch Logs.** `FilterLogEvents` is `POST /` with `Content-Type: application/x-amz-json-1.1`, `X-Amz-Target: Logs_20140328.FilterLogEvents`, and a SigV4 `Authorization` header; the body accepts `logGroupName` *or* `logGroupIdentifier`, `logStreamNames`, `filterPattern`, `startTime`, `endTime`, `limit`, `nextToken`, `interleaved`, `startFromHead`. Pages are bounded: "Each page returned can contain up to 1 MB of log events or up to 10,000 log events", and "If the results include a `nextToken`, there might be more log events available." Source: [FilterLogEvents](https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_FilterLogEvents.html). For CloudWatch Logs Insights, `StartQuery` takes `queryString` plus exactly one of `logGroupName`, `logGroupNames`, or `logGroupIdentifiers`, returns a `queryId`, and `GetQueryResults` polls it. Source: [StartQuery](https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_StartQuery.html). The adapter must therefore model a two-call flow (`StartQuery` → poll `GetQueryResults`) that the single-call backends do not need — or restrict CloudWatch to `FilterLogEvents` and document the loss of Insights aggregation.

**Datadog.** `POST /api/v2/logs/events/search` ("Search logs (POST)") requires the `apiKeyAuth` header `DD-API-KEY` and the `appKeyAuth` header `DD-APPLICATION-KEY`, or an OAuth token with `logs_read_data`. The request body carries `filter.query`, `filter.from` (`now-15m` in the documented example), `filter.to`, `page.limit` (example 25), and `sort: timestamp`; pagination is `page.cursor` with the next cursor at `meta.page.after`, and results are `data`. Source: [Datadog OpenAPI v2 specification, `securitySchemes` at lines 160240–160251 and `POST /api/v2/logs/events/search` at line 220721](https://github.com/DataDog/datadog-api-client-go/blob/master/.generator/schemas/v2/openapi.yaml) and [Datadog Logs API reference](https://docs.datadoghq.com/api/latest/logs/).

**OpenSearch.** `GET|POST /{index}/_search` and `GET|POST /_search`, with the usual `size`/`from`/query-DSL body. Source: [OpenSearch Search API](https://docs.opensearch.org/latest/api-reference/search-apis/search/). Authentication is the Security plugin's: create a token with `POST /_plugins/_security/api/apitokens` and send it as `Authorization: ApiKey os_…` — note the scheme is `ApiKey`, not `Bearer` ([OpenSearch API keys](https://docs.opensearch.org/latest/security/access-control/api-keys/)); the plugin also documents an [HTTP basic authentication](https://docs.opensearch.org/latest/security/authentication-backends/basic-auth/) backend.

**Design consequence:** four of six backends are "one GET/POST with a bearer-ish header and a JSON body"; AWS is the outlier (SigV4 + a two-call Insights flow). The adapter interface should be allowed to return `NextToken` for exactly this reason, and CloudWatch Insights should be the single documented exception to "one call, one page".

### 2.3 Inbound webhook dialects and the normalized `AlertPayload`

Four dialects, each with a documented shape:

**Prometheus Alertmanager** posts `POST` with this JSON (verbatim from the docs): `version` (currently `"4"`), `groupKey`, `truncatedAlerts`, `status` (`resolved|firing`), `receiver`, `groupLabels`, `commonLabels`, `commonAnnotations`, `externalURL`, `notification_reason`, and `alerts[]` where each alert is `{status, labels, annotations, startsAt, endsAt, generatorURL, fingerprint}`. Source: [Alertmanager configuration, `webhook_config`](https://prometheus.io/docs/alerting/latest/configuration/#webhook_config); the Go structs match: `template.Data` (`Receiver`, `Status`, `Alerts`, `GroupLabels`, `CommonLabels`, `CommonAnnotations`, `ExternalURL`) at [`alertmanager/template/template.go` 521–540](https://github.com/prometheus/alertmanager/blob/main/template/template.go) and `template.Alert` (`Status`, `Labels`, `Annotations`, `StartsAt`, `EndsAt`, `GeneratorURL`, `Fingerprint`) at 550–558, with `Message{Version, GroupKey, TruncatedAlerts}` in [`notify/webhook/webhook.go` 63–71](https://github.com/prometheus/alertmanager/blob/main/notify/webhook/webhook.go).

**Grafana Alerting** sends an Alertmanager-like payload with a different version and extra fields: the documented example contains `receiver`, `status`, `orgId`, `alerts[]` (each with `status`, `labels`, `annotations`, `startsAt`, `endsAt`, `generatorURL`, `fingerprint`, `silenceURL`, `dashboardURL`, `panelURL`, `values`), plus `groupLabels`, `commonLabels`, `commonAnnotations`, `externalURL`, `version: "1"`, `groupKey`, `truncatedAlerts`, `title`, `state`, `message`. Source: [Grafana webhook notifier](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/). A parser keyed on the Alertmanager schema mostly works, but `version` is `"1"` (not `"4"`), and `values` carries the numeric evaluation result that a Triage narrative would otherwise have to fetch.

**Datadog** webhooks are template-driven: the operator writes a payload of `$VARIABLES`, and Datadog fills them. Relevant names: `$AGGREG_KEY` ("ID to aggregate events belonging together"), `$ALERT_ID`, `$ALERT_TYPE` (`error`, `warning`, `success`, `info`), `$ALERT_CYCLE_KEY` ("ID to link events from the time an alert triggers until it resolves"), `$EVENT_TITLE`, `$EVENT_MSG`, `$EVENT_TYPE`, `$TAGS` (comma-separated), `$TAGS[key]`, `$LINK`, `$PRIORITY`, `$DATE`/`$DATE_POSIX`, `$ID`, `$ORG_ID`, `$SNAPSHOT`. Delivery behaviour is documented too: "The timeout for any individual request is 15 seconds and missed connections are retried 5 times." Source: [Datadog webhooks integration](https://docs.datadoghq.com/integrations/webhooks/). Because the payload is operator-defined, Pagerless cannot assume field names — it must either ship a documented payload template for users to paste into Datadog, or accept a `$EVENT_MSG`-only minimum and require the standard template for full fidelity.

**AWS SNS → HTTP(S)** delivers an envelope with `Type` (`Notification`; subscription and unsubscribe confirmations carry their own types), `MessageId`, `TopicArn`, `Subject` (optional), `Message`, `Timestamp`, `SignatureVersion`, `Signature` ("Base64-encoded SHA1withRSA or SHA256withRSA signature of the `Message`, `MessageId`, `Subject` (if present), `Type`, `Timestamp`, and `TopicArn` values"), `SigningCertURL`, `UnsubscribeURL`, optional `MessageAttributes`. Source: [Amazon SNS HTTP/HTTPS notification JSON format](https://docs.aws.amazon.com/sns/latest/dg/http-notification-json.html). The subscription-confirmation message is a separate documented shape: [HTTP/HTTPS subscription confirmation JSON](https://docs.aws.amazon.com/sns/latest/dg/http-subscription-confirmation-json.html).

Inside SNS, a CloudWatch alarm notification is JSON in the `Message` string. The alarm event shape itself is documented for EventBridge: `detail-type: "CloudWatch Alarm State Change"`, `source: "aws.cloudwatch"`, `resources: ["arn:aws:cloudwatch:…:alarm:Name"]`, and `detail` with `alarmName`, `state` (`{value, reason, timestamp}`), `previousState`, `configuration`. Source: [CloudWatch alarm state change events](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/cloudwatch-and-eventbridge.html). Recommendation: support **EventBridge → API destination** as the AWS path (stable, verifiable shape) and treat raw SNS `Message` parsing as best-effort.

**Normalized contract** (proposal; every field maps to a cited source above):

```json
{
  "source": "alertmanager|grafana|datadog|aws-sns|kubernetes",
  "receivedAt": "2026-11-18T10:00:00Z",
  "status": "firing|resolved",
  "fingerprint": "sha256:…",
  "groupKey": "…",
  "title": "…",
  "description": "…",
  "severity": "critical|warning|info",
  "labels": {"namespace": "…", "deployment": "…", "container": "…", "pod": "…"},
  "subject": {"kind": "Deployment", "namespace": "shop", "name": "checkout"},
  "startsAt": "…",
  "endsAt": "…",
  "generatorURL": "…",
  "raw": { /* the original body, capped, for the audit trail */ }
}
```

`fingerprint` is taken from the sender when it is stable and documented (`alerts[].fingerprint` for Alertmanager/Grafana, `$AGGREG_KEY`/`$ALERT_CYCLE_KEY` for Datadog, `MessageId`+`TopicArn` for SNS), and computed from `§1.4`'s tuple when it is not.

`POST /v1/webhooks/alerts` should keep the existing receiver's guarantees — POST only, bearer token with constant-time comparison (`internal/alerts/receiver.go:84–104`), 1 MiB cap (`:41`), and 200/400/500 semantics chosen so the sender's own retry policy does the retrying (`:78–81` documents exactly that rationale).

**Signature verification duties, per sender:**

| Sender | What can be verified | Source |
| --- | --- | --- |
| Alertmanager | nothing cryptographic in the payload; use a bearer token or mTLS | payload has no signature field ([webhook_config](https://prometheus.io/docs/alerting/latest/configuration/#webhook_config)) |
| Grafana | optional HMAC: the contact point has "HMAC signature configuration options" and "You can secure your webhook notifications using HMAC signatures to verify the authenticity and integrity of the requests"; the payload itself carries no signature | [Grafana webhook notifier](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/) |
| Datadog | optional custom headers; no body signature documented | [Datadog webhooks](https://docs.datadoghq.com/integrations/webhooks/) |
| AWS SNS | `Signature` over the canonical fields, `SigningCertURL`, `SignatureVersion` | [SNS JSON format](https://docs.aws.amazon.com/sns/latest/dg/http-notification-json.html) |

### 2.4 MCP server or built-in adapters?

MCP standardizes two transports: `stdio` ("the server MUST NOT write anything to its stdout that is not a valid MCP message") and Streamable HTTP, which "replaces the HTTP+SSE transport from protocol version 2024-11-05" and "uses HTTP POST and GET requests … [with] optional … Server-Sent Events (SSE) to stream multiple server messages". Source: [MCP Transports, 2025-06-18](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports).

| | External MCP server (`mcp.url`) | Built-in adapter |
| --- | --- | --- |
| Helm surface | one URL + optional Secret reference; no image, no version skew | one image, adapter list in `values.yaml`, credentials via existing `existingSecret` |
| Protocol lifetime | server and client must agree on a spec revision; HTTP+SSE is already deprecated | none: HTTP APIs, versioned by the vendor |
| Failure domain | a second process to schedule, probe, and page on | in-process, covered by the server's own readiness |
| Query shape control | whoever wrote the server decides which fields are exposed | Pagerless controls timeouts, limits, redaction |
| Supply chain | an extra image to pin and scan | none |

Recommendation: ship built-in adapters for the six backends above, and treat an optional `mcp.url` as a **bridge for backends Pagerless does not implement** — not as the primary path. The repository already runs MCP for evidence (`charts/pagerless/templates/mcp.yaml`, `deploy/mcp/`, policy assertions in `internal/server/mcp_policy_test.go:33–44`), so there is precedent for the pattern; the argument here is only about which path owns the common cases.

---

## 3. Mapping a failing cluster resource back to a repository file

### 3.1 The labels and annotations that already exist on the object

**Argo CD.** The default tracking method is the **annotation** `argocd.argoproj.io/tracking-id`, whose value is `<application>:<group>/<kind>:<namespace>/<name>` (example: `my-app:apps/Deployment:default/my-deployment`). The three documented methods are: `annotation` (default), `annotation+label` (annotation tracks; `app.kubernetes.io/instance` is informational only and "not used for tracking purposes"), and `label` (tracks by `app.kubernetes.io/instance`). Source: [Argo CD Resource Tracking](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/).

Two consequences. First, `argocd.argoproj.io/instance` — the label the ticket names — is only present when the installation selected `label` or `annotation+label`; a default Argo CD 2.x install leaves it off. Second, the tracking-id encodes the **application name**, which is the key needed to find the Application object and therefore `spec.source.path` / `spec.source.repoURL`.

**Flux.** `kustomize-controller` stamps ownership labels in the form `<owner.group>/name` and `<owner.group>/namespace`. The implementation is `SetOwnerLabels` in `fluxcd/pkg/ssa`: "The ownership labels are in the format: `<owner.group>/name: <name>`, `<owner.group>/namespace: <namespace>`". Source: [`fluxcd/pkg/ssa/manager.go`, lines 60–84](https://github.com/fluxcd/pkg/blob/main/ssa/manager.go). The controller constructs the manager with `Group: kustomizev1.GroupVersion.Group` and calls `resourceManager.SetOwnerLabels(objects, obj.GetName(), obj.GetNamespace())`. Source: [`fluxcd/kustomize-controller`, `kustomization_controller.go` lines 494–497](https://github.com/fluxcd/kustomize-controller/blob/main/internal/controller/kustomization_controller.go).

So the concrete keys to read are `kustomize.toolkit.fluxcd.io/name` and `kustomize.toolkit.fluxcd.io/namespace`, pointing at the `Kustomization` object.

**Helm.** Helm itself stamps every managed object with the label `app.kubernetes.io/managed-by: Helm` and the annotations `meta.helm.sh/release-name` and `meta.helm.sh/release-namespace`. Source: [`helm/helm`, `pkg/action/validate.go` v3.19.2, constants at lines 34–37, `setMetadataVisitor` at 140–173](https://github.com/helm/helm/blob/v3.19.2/pkg/action/validate.go). Flux's Helm controller is a different case: no `SetOwnerLabels` call appears in its HelmRelease reconciler ([`fluxcd/helm-controller`, `internal/controller/helmrelease_controller.go`](https://github.com/fluxcd/helm-controller/blob/main/internal/controller/helmrelease_controller.go)), so the reliable provenance for a Helm-rendered object is Helm's own `meta.helm.sh/*` annotations plus `app.kubernetes.io/managed-by=Helm`, not a `helm.toolkit.fluxcd.io/*` label.

### 3.2 Resolution order (most authoritative first)

1. **`argocd.argoproj.io/tracking-id`** → split on `:` → application name → `kubectl get application <name> -n argocd -o json` (or the Argo CD API) → `spec.source.repoURL`, `spec.source.path`, `spec.source.targetRevision`. This is exact; do not guess.
2. **`app.kubernetes.io/instance`** (only when the installation tracks by label) → same Application lookup, but the value is truncated to 63 characters and can collide across Applications, so require a repository match before trusting it.
3. **`kustomize.toolkit.fluxcd.io/name` + `/namespace`** → `Kustomization.spec.path` and `.spec.sourceRef` → `GitRepository.spec.url` and `.spec.ref`. Exact for Kustomize-managed resources.
4. **`meta.helm.sh/release-name` + `/namespace`** → `HelmRelease.spec.chart`/`spec.chartRef` and `.spec.values`; the file to patch is the `HelmRelease` manifest or its referenced `valuesFrom` ConfigMap/Secret — **not** the rendered chart, which lives in the chart repository.
5. **Heuristics**, only when 1–4 fail; see §3.3.

Everything else is guesswork and must be labelled as such in the Page, because a wrong file produces a plausible-looking PR against the wrong workload.

### 3.3 Fallback heuristics when nothing is labelled

The workspace files that exist in a repository:

- `kustomization.yaml` / `kustomization.yml` marks a Kustomize directory; an overlay is a directory whose `kustomization.yaml` has `resources:` pointing at `../base`.
- `Chart.yaml` marks a Helm chart; `values.yaml` is its default values.
- Plain YAML: any file whose `kind:`/`metadata.name` match.

GitHub's read APIs constrain the search:

| API | What it gives | Hard limits |
| --- | --- | --- |
| `GET /repos/{owner}/{repo}/git/trees/{tree_sha}?recursive=1` | every path + blob SHA in one call | "The limit for the tree array is 100,000 entries with a maximum size of 7 MB when using the recursive parameter"; on overflow `truncated: true` and you must "use the non-recursive method of fetching trees, and fetch one sub-tree at a time" |
| `GET /repos/{owner}/{repo}/contents/{path}` | file bytes | "1 MB or smaller: All features of this endpoint are supported. Between 1-100 MB: Only the raw or object custom media types … Greater than 100 MB: This endpoint is not supported." |
| `GET /search/code?q=…` | content matches | requires authentication; "10 requests per minute"; "up to 1,000 results"; "Only the default branch is considered"; "Only files smaller than 384 KB are searchable" |
| `POST /repos/{owner}/{repo}/pulls` | opens the PR | requires write access to the head branch; `Pull requests: write` |

Sources: [Git Trees](https://docs.github.com/en/rest/git/trees), [Repository contents](https://docs.github.com/en/rest/repos/contents), [Search](https://docs.github.com/en/rest/search/search), [Pull requests](https://docs.github.com/en/rest/pulls/pulls).

Practical algorithm:

1. One recursive Trees call on the target branch. If `truncated`, fall back to per-directory trees along `argocd`/`flux`-reported paths only.
2. Filter to candidate manifests: `*.yaml`/`*.yml` whose path contains `kustomization`, `Chart.yaml`, `values.yaml`, or the workload name.
3. Walk the filtered set through Contents (cheap: manifests are small) and match on `apiVersion`+`kind`+`metadata.name`+`metadata.namespace`.
4. Use Code Search only as a last resort, with a query built from the exact object identity, and treat the 384 KB/indexing limits as a documented source of false negatives.
5. Record **why** a file was chosen (`tracking-id`, `flux-label`, `helm-annotation`, `name-match`, `content-match`) in the Page, and refuse to auto-open a PR on a `name-match` or weaker until a human confirms.

The repository already records the *base* a change was rendered against: `api/v1alpha1/proposal_types.go:108–118` (`RenderedChange{Path, Patch}`) and `:120–129` (`ProposalBase{Commit, WorkloadGeneration}`), with "A Proposal whose base changed is a Stale Proposal". That is exactly the right structure for PR generation — the missing piece is populating `Path` from the mapping above instead of from a Sandbox-relative guess.

### 3.4 Pull request mechanics

The existing Executor already implements the PR half: `POST /repos/{owner}/{repo}/pulls` with `{title, head, base, body}` (`internal/executor/github.go:111–122`), a 422 "already exists" mapped to `ErrPullRequestExists` (`:116–117`), lookup by head branch (`:97–107`), and auto-merge through GraphQL because "only the GraphQL API offers" it (`:124–149`). Nothing about that needs to change; what changes is where the branch content comes from.

The write path, per GitHub's docs:

1. `POST /repos/{owner}/{repo}/git/refs` with `{"ref": "refs/heads/pagerless/<fingerprint>", "sha": "<base commit>"}` — requires `Contents: write`; "You are unable to create new references for empty repositories" and the ref "must start with `refs` and have at least two slashes". Source: [Git references](https://docs.github.com/en/rest/git/refs).
2. `PUT /repos/{owner}/{repo}/contents/{path}` with `message`, Base64 `content`, and `sha` (required when updating) — requires `Contents: write`; the same endpoint also needs `Workflows: write` to touch `.github/workflows`. Source: [Create or update file contents](https://docs.github.com/en/rest/repos/contents).
3. `POST /repos/{owner}/{repo}/pulls` — requires `Pull requests: write`. Source: [Create a pull request](https://docs.github.com/en/rest/pulls/pulls).
4. Optionally `GET /repos/{owner}/{repo}/commits/{ref}/check-runs` (or the combined status API) to wait for CI; the repository already does the equivalent against a local test server (`internal/server/execution_test.go`, `env.GitHub.CompleteChecks`).

### 3.5 Least-privilege GitHub credentials

For a fine-grained PAT scoped to the one target repository:

| Permission | Level | Why |
| --- | --- | --- |
| `Contents` | write | create the ref, create/update the file |
| `Pull requests` | write | open (and optionally update) the PR |
| `Metadata` | read | the only level GitHub offers for metadata |

There is no documented permission for "read the tree" distinct from `Contents` — the Trees endpoint states: "The fine-grained token must have the following permission set: `Contents` repository permissions (read)" ([Git Trees](https://docs.github.com/en/rest/git/trees)). `Metadata` is documented as `read` only in GitHub's permission tables, and its canonical token-creation example pairs the requested scopes with `metadata:read` ([Managing your personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)). No `Issues`, `Actions`, `Administration`, or `Workflows` permission is needed unless the generated PR edits workflow files.

A GitHub App installation token is equivalent in scope but preferable operationally: tokens expire in an hour, are minted per installation, and the App's permission set is auditable. Both token types are listed as supported on every endpoint used above ("GitHub App user access tokens", "GitHub App installation access tokens", "Fine-grained personal access tokens").

**Non-negotiable:** the token must never be mounted into the server that renders a Page (`internal/server/executor_manifests_test.go:100–103` already asserts that "the server mounts the write-scoped GitHub token, which only the Executor may hold", and that the Executor must not mount it either way round). v2 keeps that split: the watcher and the model never hold a write credential.

### 3.6 Validating a change without cluster write RBAC

Server-side dry run is not available to a read-only identity — authorization runs before the dry-run short-circuit (§1.3). The options, in order of strength:

1. **CI as the validating authority.** The PR triggers the repository's own pipeline (kubeconform/kubeval/`helm template`/`kustomize build`/`kubectl apply --dry-run=client`), and Pagerless never validates against the cluster. This is the only option consistent with "zero cluster write RBAC", and it is also the only one that validates the *repository*, which is the artifact being changed.
2. **Client-side schema validation inside Pagerless**, using downloaded OpenAPI schemas rather than a cluster write. Cheap and offline-capable, but it cannot catch admission policies, quota, or drift.
3. **Server-side dry run with a deliberately separate, write-scoped credential** — which reintroduces exactly the permission the v2 map rules out. If this is ever chosen, it must be a distinct ServiceAccount and a distinct RBAC grant, documented as an exception, not folded into the watcher's role.

Recommendation: option 1 as the product behaviour, option 2 as an advisory pre-check, option 3 explicitly out of scope.

---

## 4. Packaging and distributing `kubectl page`

### 4.1 Naming: the binary is `kubectl-page`, the plugin is `page`

`kubectl` discovers plugins by executable name: "Create an executable binary, named `kubectl-foo` (for example) to have a plugin that can be invoked as `kubectl foo`." Source: [Krew, Introduction to plugin development](https://krew.sigs.k8s.io/docs/developer-guide/develop/plugin-development/). Krew's manifest makes the same statement from the other side: the manifest's `name` "defines how the plugin is invoked, for example: `kubectl restart`", and "`name` must match the filename of the manifest". Source: [Krew plugin manifest](https://krew.sigs.k8s.io/docs/developer-guide/plugin-manifest/).

Therefore: repository binary `kubectl-page` → plugin name `page` → index file `plugins/page.yaml` → user command `kubectl page`.

The naming guide adds two rules that apply: don't repeat `kube`/`kubernetes` in the name ("Plugin names should not include 'kube-' or 'kubernetes-' prefixes"), and prefer full resource words over acronyms. Source: [Krew naming guide](https://krew.sigs.k8s.io/docs/developer-guide/develop/naming-guide/). `page` satisfies both; `kubectl-triage` (the existing binary, `cmd/kubectl-triage/main.go:1–2`) does not collide with anything built in, and v2 can keep both names if `page` is added alongside it.

### 4.2 Krew manifest, annotated

```yaml
# plugins/page.yaml in kubernetes-sigs/krew-index
apiVersion: krew.googlecontainertools.github.com/v1alpha2
kind: Plugin
metadata:
  name: page                    # must match the filename: plugins/page.yaml
spec:
  version: v2.0.0               # semver, 'v' prefix required
  homepage: https://github.com/a-hagi613/pagerless
  shortDescription: Open a Pagerless remediated page for a failing workload
  description: |
    Starts and inspects Pagerless Pages from the terminal. Pagerless watches
    failing workloads in a cluster and proposes a GitHub pull request.
  caveats: |
    Requires a cluster with the Pagerless server installed (helm install).
  platforms:
    - selector:
        matchLabels:
          os: linux
          arch: amd64
      uri: https://github.com/a-hagi613/pagerless/releases/download/v2.0.0/kubectl-page_2.0.0_linux_amd64.tar.gz  # future artifact
      sha256: <sha256 of that archive>
      bin: kubectl-page
      files:
        - from: kubectl-page
          to: .
        - from: LICENSE
          to: .
    - selector:
        matchLabels: {os: linux, arch: arm64}
      uri: …/kubectl-page_2.0.0_linux_arm64.tar.gz
      sha256: …
      bin: kubectl-page
      files: [{from: kubectl-page, to: .}, {from: LICENSE, to: .}]
    - selector:
        matchLabels: {os: darwin, arch: amd64}
      uri: …/kubectl-page_2.0.0_darwin_amd64.tar.gz
      sha256: …
      bin: kubectl-page
      files: [{from: kubectl-page, to: .}, {from: LICENSE, to: .}]
    - selector:
        matchLabels: {os: darwin, arch: arm64}
      uri: …/kubectl-page_2.0.0_darwin_arm64.tar.gz
      sha256: …
      bin: kubectl-page
      files: [{from: kubectl-page, to: .}, {from: LICENSE, to: .}]
```

Field semantics, all from the [Krew plugin manifest reference](https://krew.sigs.k8s.io/docs/developer-guide/plugin-manifest/): `version` is "a valid semantic version string … the prefix 'v' is required"; `platforms[].selector` is a label selector over `os`/`arch` (the docs' own example uses `matchExpressions` with `key: "os"` and `operator: "In"`); `uri` is the `.zip` or `.tar.gz` archive URL; `sha256` is "the sha256sum of the url (archive file)"; `files` lists `from`→`to` extractions; `bin` is "the path to the plugin executable among extracted files". `shortDescription` and `description` are required.

Submission: open a PR against `kubernetes-sigs/krew-index` adding the manifest "to the `plugins/` directory" after tagging a semver release, opening the source, and including the LICENSE in the archive — the checklist also asks that you "extract the LICENSE file during the plugin installation", which the `files` entry above does. Source: [Submitting plugins to Krew](https://krew.sigs.k8s.io/docs/developer-guide/release/new-plugin/).

### 4.3 GoReleaser: cross-compile, archive, checksum, publish

GoReleaser's Go builder defaults to `goos: [darwin, linux, windows]` and `goarch: [386, amd64, arm64]`, with per-target `goarm`/`goamd64` and an `overrides` list for `ldflags`/`env` per platform; `env: [CGO_ENABLED=1]` is set through the same block. Source: [GoReleaser Go builder](https://goreleaser.com/customization/builds/go/).

Archives default to `format: tar.gz` and take `name_template`, `formats` (plural; the singular `format` is deprecated), `ids`, and `builds_info`. Source: [GoReleaser Archives](https://goreleaser.com/customization/archive/). The default `name_template` is `{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}`.

Checksums default to `algorithm: sha256` ("Accepted options … Default: `sha256`") with `name_template: "{{ .ProjectName }}_checksums.txt"`. Source: [GoReleaser Checksums](https://goreleaser.com/customization/checksum/).

Recommended `.goreleaser.yaml` (proposal, built from the documented keys):

```yaml
version: 2
project_name: kubectl-page

builds:
  - id: kubectl-page
    main: ./cmd/kubectl-page
    binary: kubectl-page
    env: [CGO_ENABLED=0]
    goos: [linux, darwin]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w -X github.com/a-hagi613/pagerless/internal/version.Version={{ .Version }}

archives:
  - id: kubectl-page
    formats: [tar.gz]
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}
    files:
      - LICENSE
      - README.md

checksum:
  name_template: "{{ .ProjectName }}_checksums.txt"
  algorithm: sha256

krews:
  - name: page
    homepage: https://github.com/a-hagi613/pagerless
    short_description: Open a Pagerless remediated page for a failing workload
    repository:
      owner: a-hagi613
      name: krew-index
      branch: master
    commit_author:
      name: pagerless-release-bot
      email: bot@example.com

homebrew_casks:
  - name: kubectl-page
    homepage: https://github.com/a-hagi613/pagerless
    repository:
      owner: a-hagi613
      name: homebrew-tap
    binaries:
      - kubectl-page
```

Supporting details: the Krew publisher's section key is `krews:` and it accepts `name`, `homepage`, `short_description`, `description`, `caveats`, `skip_upload`, and `repository` (owner/name/branch/token, with optional `pull_request`) — the documented page is [GoReleaser Krew Plugin Manifests](https://goreleaser.com/customization/krew/). The Homebrew section is `homebrew_casks:` "Since v2.10", with `repository`, `directory` (default `Casks`), `caveats`, `dependencies`, and `commit_msg_template`; the older `brews:` formula generator is labelled **Homebrew (deprecated)** in the documentation navigation, and `goreleaser.com/customization/homebrew/` now serves the cask page. Sources: [GoReleaser Homebrew Casks](https://goreleaser.com/customization/homebrew_casks/), [GoReleaser Homebrew (deprecated)](https://goreleaser.com/customization/homebrew/).

Krew's own project strongly recommends automating the index update: "krew-release-bot is a Github Action to automatically bump the version in krew-index repo every time you push a new git tag … It requires no secrets … It creates your plugin manifest dynamically from a template you write." Source: [Krew, Automating plugin updates](https://krew.sigs.k8s.io/docs/developer-guide/release/automating-updates/). Either the bot or GoReleaser's `krews:` publisher is acceptable; choose one, not both, or the index gets two competing PRs per tag.

### 4.4 Homebrew: tap or cask

Tap mechanics: `brew tap <user>/<repository>` clones `https://github.com/<user>/homebrew-<repository>`; "On GitHub, a repository must be named `homebrew-<repository>` to use the one-argument form of `brew tap`"; and the trust-preserving install form is fully qualified, `brew install user/repository/formula`. Source: [Homebrew Taps](https://docs.brew.sh/Taps).

A formula for a prebuilt binary is small and stable: `url` to the release tarball, `sha256`, `version`, `license`, and `bin.install`. The cookbook documents `bin.install "foo"` as the install helper and `on_macos`/`on_linux`/`on_arm`/`on_intel` blocks for OS- and architecture-specific components. Source: [Homebrew Formula Cookbook](https://docs.brew.sh/Formula-Cookbook).

A cask is also valid: the Cask Cookbook lists a `binary` stanza among the artifact stanzas, alongside `sha256`, `version`, `url`, and `depends_on`. Source: [Homebrew Cask Cookbook](https://docs.brew.sh/Cask-Cookbook).

Recommendation: publish a **formula** in `a-hagi613/homebrew-tap` for `brew install a-hagi613/tap/kubectl-page` (formulae are the natural shape for a CLI binary and `brew audit` is well-trodden), and let GoReleaser's `homebrew_casks` remain unused unless the tap later needs a macOS `.app`.

### 4.5 Release sequence and verification

1. Tag `v2.0.0`; GoReleaser builds `linux/{amd64,arm64}` and `darwin/{amd64,arm64}`, uploads four `tar.gz` archives, and writes `kubectl-page_checksums.txt` (SHA-256 by default).
2. Krew manifest `plugins/page.yaml` carries the `sha256` of each archive — taken from that checksum file, never computed by hand.
3. Verify locally before announcing: `kubectl krew install --manifest=plugins/page.yaml` (documented at [Testing locally](https://krew.sigs.k8s.io/docs/developer-guide/testing-locally/)), `kubectl page --help`, `brew install --build-from-source ./Formula/kubectl-page.rb` or `brew audit --strict`.
4. Users install with `kubectl krew install page` and `brew install a-hagi613/tap/kubectl-page`; both resolve to the same GitHub Release artifacts and the same SHA-256.

### 4.6 Current repository state

`cmd/kubectl-triage/main.go:1–2` documents the existing convention: "Command kubectl-triage is the kubectl plugin that starts, lists, and shows Triages. kubectl runs it as `kubectl triage`." There is no `.goreleaser.yaml`, no `plugins/` index manifest, and no Homebrew formula in the tree today; `charts/pagerless/` is the only distribution channel (`Chart.yaml`, `values.yaml`, `templates/`). Adding `kubectl page` therefore means: a new `cmd/kubectl-page` entry point, one GoReleaser config, one Krew manifest template, and one formula — no change to the chart.

---

## 5. Decisions this research supports

1. **Bind both Event APIs, read `regarding` on `events.k8s.io/v1`.** The ticket's `involvedObject` assumption is a real bug in waiting (§1.1).
2. **Fingerprint without the message**, over cluster/kind/namespace/name/uid/reason/container, domain-separated like `internal/proposal/hash.go:21` (§1.4).
3. **Persist cooldown and suppression on the Page**, because Events live one hour by default (§1.5).
4. **One Active Page per fingerprint, chosen by `CanonicalActive`-style select-then-sort**, not by relying on a webhook refusal (§1.5).
5. **Drop the `update` verb on `deployments, statefulsets`** from `charts/pagerless/templates/rbac.yaml:198–204`; server-side dry run is an authorized write, so validation moves to CI (§1.3, §3.6).
6. **Six read-only adapters, one interface with an opaque `NextToken`**, CloudWatch Insights documented as the only two-call flow (§2.2).
7. **Four webhook dialects into one `AlertPayload`**; verify SNS signatures, bearer-token everything else (§2.3).
8. **Prefer built-in adapters over an MCP hop**; keep `mcp.url` as the extension point (§2.4).
9. **Resolve provenance in the order tracking-id → instance label → flux labels → helm annotations → heuristics**, and record the reason on the Page (§3.2).
10. **Scopes: `Contents: write`, `Pull requests: write`, `Metadata: read`** — and keep the token out of the server process (§3.5).
11. **Publish as Krew plugin `page` + Homebrew formula in a `homebrew-` tap**, driven by one GoReleaser config (§4).

## 6. Open questions

- **Does the Page CRD own the cooldown, or a separate `FailureRecord`?** A Page deleted on resolution loses its cooldown history, so a flapping workload re-pages immediately after each recovery.
- **How should a Kubernetes-Event Page and an Alertmanager Triage share identity?** Both can target one Deployment; §1.5 proposes fingerprint-as-join-key, but the `Triage`/`Page` split is undecided.
- **Which severity ladder applies to a suppressed count?** The `client-go` constants (§1.5) give a rate, not a severity mapping.
- **Is `kubectl page` a second plugin or a rename of `kubectl-triage`?** The chart, docs and tests all assume `kubectl triage` today (`cmd/kubectl-triage/main.go`, `internal/cli/`).

## 7. Source index

Kubernetes and controller-runtime:
- https://github.com/kubernetes/api/blob/master/core/v1/types.go
- https://github.com/kubernetes/api/blob/master/events/v1/types.go
- https://github.com/kubernetes/api/blob/master/apps/v1/types.go
- https://github.com/kubernetes/api/blob/master/batch/v1/types.go
- https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#event-v1-core
- https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#event-v1-events-k8s-io
- https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#read-log-pod-v1-core
- https://kubernetes.io/docs/reference/access-authn-authz/rbac/
- https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors/
- https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run
- https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/
- https://kubernetes.io/docs/reference/command-line-tools-reference/kube-apiserver/
- https://github.com/kubernetes/client-go/blob/master/tools/record/events_cache.go
- https://github.com/kubernetes/client-go/blob/master/util/workqueue/queue.go
- https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/predicate/predicate.go
- https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/builder/controller.go
- https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/cache/cache.go

Observability and alerting:
- https://prometheus.io/docs/prometheus/latest/querying/api/
- https://prometheus.io/docs/guides/basic-auth/
- https://prometheus.io/docs/alerting/latest/configuration/#webhook_config
- https://github.com/prometheus/alertmanager/blob/main/notify/webhook/webhook.go
- https://github.com/prometheus/alertmanager/blob/main/template/template.go
- https://grafana.com/docs/loki/latest/reference/loki-http-api/
- https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/
- https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_FilterLogEvents.html
- https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_StartQuery.html
- https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/cloudwatch-and-eventbridge.html
- https://docs.datadoghq.com/api/latest/logs/
- https://github.com/DataDog/datadog-api-client-go/blob/master/.generator/schemas/v2/openapi.yaml
- https://docs.datadoghq.com/integrations/webhooks/
- https://docs.opensearch.org/latest/api-reference/search-apis/search/
- https://docs.opensearch.org/latest/security/access-control/api-keys/
- https://docs.aws.amazon.com/sns/latest/dg/http-notification-json.html
- https://docs.aws.amazon.com/sns/latest/dg/http-subscription-confirmation-json.html
- https://modelcontextprotocol.io/specification/2025-06-18/basic/transports

GitOps and GitHub:
- https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/
- https://github.com/fluxcd/pkg/blob/main/ssa/manager.go
- https://github.com/fluxcd/kustomize-controller/blob/main/internal/controller/kustomization_controller.go
- https://github.com/helm/helm/blob/v3.19.2/pkg/action/validate.go
- https://docs.github.com/en/rest/git/trees
- https://docs.github.com/en/rest/git/refs
- https://docs.github.com/en/rest/repos/contents
- https://docs.github.com/en/rest/pulls/pulls
- https://docs.github.com/en/rest/search/search
- https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens
- https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens

Packaging:
- https://krew.sigs.k8s.io/docs/developer-guide/plugin-manifest/
- https://krew.sigs.k8s.io/docs/developer-guide/develop/plugin-development/
- https://krew.sigs.k8s.io/docs/developer-guide/develop/naming-guide/
- https://krew.sigs.k8s.io/docs/developer-guide/release/new-plugin/
- https://krew.sigs.k8s.io/docs/developer-guide/release/automating-updates/
- https://krew.sigs.k8s.io/docs/developer-guide/testing-locally/
- https://goreleaser.com/customization/builds/go/
- https://goreleaser.com/customization/archive/
- https://goreleaser.com/customization/checksum/
- https://goreleaser.com/customization/krew/
- https://goreleaser.com/customization/homebrew_casks/
- https://goreleaser.com/customization/homebrew/
- https://docs.brew.sh/Taps
- https://docs.brew.sh/Formula-Cookbook
- https://docs.brew.sh/Cask-Cookbook

Repository files:
- `charts/pagerless/templates/rbac.yaml`
- `internal/alerts/receiver.go`
- `internal/triage/active.go`
- `internal/proposal/hash.go`
- `internal/patchresources/validator.go`
- `internal/controller/triage_controller.go`
- `internal/controller/verification.go`
- `internal/server/server.go`
- `internal/executor/github.go`
- `internal/server/mcp_policy_test.go`
- `api/v1alpha1/proposal_types.go`
- `cmd/kubectl-triage/main.go`
