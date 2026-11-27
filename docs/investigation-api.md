# Investigation API

Both clients use JSON under `/api/v1`. An operator token supplied by the installer grants access to the installation. CLI requests send an Authorization bearer header. The browser exchanges the same token at `POST /api/v1/session` with a JSON `token` field for an HttpOnly, SameSite=Strict session cookie. Require same-origin browser writes, reject untrusted origins, and keep secrets out of URLs and browser storage.

| Method and path | Request and response |
|---|---|
| POST /pages | Send message, optional namespace and target. Return HTTP 201 with pageId and turnId. |
| GET /pages | Return an object with a pages array of summaries, newest activity first. |
| GET /pages/{id} | Return a page summary with a messages array and lastEventId. |
| POST /pages/{id}/turns | Send message. Return HTTP 201 with pageId and turnId. |
| POST /pages/{id}/stop | Stop the active turn. Return HTTP 200 with status. Repeated calls are safe. |
| GET /pages/{id}/events?after=0 | Return an events array in sequence order. With Accept: text/event-stream, replay then follow with SSE id equal to seq and JSON data equal to the event. Support Last-Event-ID. |
| GET /pages/{id}/evidence/{callId} | Return sanitized tool evidence including callId, tool, arguments, result, status, durationMs, redacted and truncated. |
| GET /pages/{id}/diff | Return files containing path and diff, and optional prUrl. An empty files array means no proposal. |

Use camelCase wire names. IDs are opaque strings except for per-page event sequence numbers, which start at one. A page summary includes id, title, status, updatedAt and activeTurnId (nullable). Messages contain id, turnId, role, content, createdAt and citations (callId and optional JSON pointer). Timestamps use UTC RFC 3339. Each event contains seq, pageId, turnId, type, time and data. Event types are turn.started, tool.started, tool.finished, answer, proposal and turn.finished. Tool events include callId and tool. Completed attempts carry status and durationMs. Turn statuses are running, completed, stopped, failed and interrupted. SSE reconnects resume after the last durable sequence. A client disconnect leaves the turn active. Clients send Idempotency-Key on create and reply. Reject a conflicting active turn with HTTP 409. Errors carry an error object with code and message.

For example, a tool.started event for a Service read can carry:

```json
{"seq":2,"pageId":"p1","turnId":"t1","type":"tool.started","time":"2026-01-01T00:00:01Z","data":{"callId":"c1","tool":"resources_get","arguments":{"kind":"Service","name":"checkout","namespace":"shop"}}}
```

The private agent interface runs inside the installation. Go sends POST /turn to the Python service with pageId, turnId, messages, namespace, target, tools (name, description, inputSchema), and an installation-controlled callback base URL. Authenticate both directions using an installer-provided internal bearer credential. Go owns cancellation and calls POST /turns/{turnId}/stop on Python. Python responds to POST /turn with a terminal JSON object carrying content, citations and optional proposal (baseSha, files containing path and content). During the turn, Python calls POST /internal/turns/{turnId}/tools on Go with callId, tool and arguments and receives status, result, redacted and truncated. Tool calls may read evidence only. Go validates and publishes any proposal after the agent returns. Bind callbacks and proposals to the active turn, reject late work and prevent duplicate publication after retries.

## Client request details

Create and reply accept the same object. Only message is required. Namespace and target are optional hints. A reply inherits the conversation's hints when omitted. Reject conflicting namespace hints with HTTP 400.

```json
{"message":"Why is checkout unavailable?","namespace":"shop","target":{"kind":"Deployment","name":"checkout"}}
```

The target object requires kind and name and optionally accepts namespace. Errors use `{"error":{"code":"invalid_request","message":"..."}}`. The Go service serves the dashboard at / and a minimal readiness response at GET /readyz (200 ready, 503 degraded). Clients may probe readiness for diagnostics. Make the actual authenticated request directly in normal operation. CLI version can report its local build metadata.

## Private turn contract

POST /turn requires pageId, turnId, messages, tools and callbackUrl. Each message contains role (user or assistant), content and optional turnId. Optional namespace and target use the client shapes above. Optional limits may lower the configured toolCalls, modelCalls, inputTokens and seconds ceilings. callbackUrl must equal the configured callback origin. Reject userinfo, query, fragment, unexpected paths and credential-bearing redirects.

A terminal HTTP 200 response contains status (completed, stopped or failed), content, citations, and usage (inputTokens, outputTokens, modelCalls, toolCalls). Each citation has callId and optional pointer. A completed response may include proposal with baseSha and files containing path and content. A failed response includes error with code and message. Go must validate the active turn before accepting a terminal answer or publishing.

Allow concurrent pages up to the configured limit. Return 409 for a second active turn on the same page, an identical request still in progress, or reuse of a turn ID with a different payload. Distinguish these cases with codes page_busy, turn_running and idempotency_conflict. Replay a completed identical request from a bounded cache. Return 429 when global capacity is exhausted. Go owns durable turn state. After an ambiguous disconnect, retry only within a known replay window with the identical request. After agent restart or uncertain cache retention, mark the turn interrupted rather than re-executing it silently.

POST /turns/{turnId}/stop returns HTTP 200 with status stopped, finished or unknown. A repeated stop is safe. Tool callbacks return status (ok, error, denied, truncated, timed_out or budget_exhausted), result and boolean redacted and truncated fields. Repeat delivery of the same callId and payload retrieves its recorded result. A genuinely new attempt has a fresh ID.

Python GET /readyz reports status (ready or degraded) and checks containing name, ok and sanitized reason. Python owns the model tool-calling check. Go aggregates it with Kubernetes MCP capability and GitHub repository access checks. Cache active probes with bounded refresh and backoff. Keep liveness independent.

The chart and agent share PAGERLESS_INTERNAL_TOKEN_FILE, PAGERLESS_CALLBACK_ORIGIN and PAGERLESS_AGENT_MAX_CONCURRENCY. Use PAGERLESS_MODEL_BASE_URL, PAGERLESS_MODEL_NAME and PAGERLESS_MODEL_API_KEY_FILE for the model. Limits use PAGERLESS_BUDGET_TOOL_CALLS, PAGERLESS_BUDGET_MODEL_CALLS, PAGERLESS_BUDGET_INPUT_TOKENS and PAGERLESS_BUDGET_SECONDS.

## MCP automation

The server serves an MCP endpoint at `POST /mcp`, authenticated with the installation's
`trigger-token` (or the operator token when the key is absent). It exposes one tool:

- `pagerless_investigate` — arguments `question` (required), `namespace`, `target`
  (`{kind, name, namespace?}`) and an optional `idempotencyKey`. It creates a page and a turn, then
  returns `{"pageId":"...","turnId":"...","url":"/pages/..."}`. Reuse the alert fingerprint as the
  idempotency key so a retry does not open a second page.

The same payload may be sent as the `notifications/pagerless/investigate` JSON-RPC notification.
Notifications have no response body; the server answers HTTP 202 and records a handler failure in
its log. A bridge maps a Grafana or Prometheus alert to this payload.

The observability integrations expose these read tools through the same admitted-tool registry:

| Integration | Tools |
| --- | --- |
| Prometheus | `prometheus_query`, `prometheus_alerts`, `prometheus_targets` |
| Grafana | `grafana_alerts`, `grafana_dashboards`, `grafana_dashboard`, `grafana_datasources` |

The server admits an explicit allowlist per integration. A tool the provider adds later is not
visible until it is named in the server. Grafana data source responses are stripped of
`secureJsonData`, `password`, `token` and `basicAuthPassword` before they enter the transcript.
