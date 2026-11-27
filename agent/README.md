# Pagerless investigation agent

A long-running, private HTTP service. The Go service owns the public API, durable history,
tool policy and publication. This service runs one LangGraph investigation turn: it asks an
OpenAI-compatible Chat Completions model what to read next, executes that read through the
Go callback, validates the model's citations, and returns one terminal JSON object.

It holds no Kubernetes credential, no GitHub credential and no shell. It cannot list the
cluster or the repository: the forwarded tool catalogue is the complete set it may call,
and the Go policy decides whether each call is allowed.

## Endpoints

| Method and path | Purpose |
|---|---|
| `POST /turn` | Run one turn. Requires the internal bearer credential. |
| `POST /turns/{turnId}/stop` | Cancel one active turn. Repeated calls are safe. |
| `GET /readyz` | `{status, checks}` with the model tool-call check; 200 ready, 503 degraded. No auth. |
| `GET /healthz`, `GET /livez` | Process liveness only. No auth, no model call. |

### `POST /turn`

```json
{
  "pageId": "p1",
  "turnId": "t1",
  "messages": [
    {"role": "user", "content": "Why is checkout timing out?", "turnId": "t1"}
  ],
  "namespace": "shop",
  "target": {"kind": "Deployment", "name": "checkout", "namespace": "shop"},
  "tools": [
    {
      "name": "resources_get",
      "description": "Read one allowed Kubernetes object.",
      "inputSchema": {"type": "object", "properties": {}}
    }
  ],
  "callbackUrl": "http://pagerless-server.pagerless.svc:8080",
  "limits": {"toolCalls": 4, "modelCalls": 4, "inputTokens": 100000, "seconds": 60}
}
```

`messages` is the stored conversation; the last message must be the current non-blank user
message. `namespace` and `target` are optional data hints. `target` requires `kind` and
`name`. Conflicting namespace hints are `400 invalid_request`. `limits` may only lower the
configured ceilings. `callbackUrl` must have exactly the configured origin: no credentials,
path, query or fragment. Unknown request fields are ignored for forward compatibility.

Terminal response (HTTP 200):

```json
{
  "status": "completed",
  "content": "Checkout calls an upstream that is not accepting connections.",
  "citations": [{"callId": "c-1", "pointer": "/data/items/0"}],
  "proposal": {
    "baseSha": "0123456789abcdef0123456789abcdef01234567",
    "files": [{"path": "apps/shop/deployment.yaml", "content": "kind: Deployment\n"}]
  },
  "usage": {"inputTokens": 1200, "outputTokens": 180, "modelCalls": 2, "toolCalls": 1}
}
```

`status` is `completed`, `stopped` or `failed`. A failed response carries
`error: {code, message}` and no proposal. `citations` name a call result received in this
turn and an optional JSON Pointer into it; every citation is resolved locally. A proposal
requires at least one citation and may name at most five existing repository files.
Repository paths may not be absolute, traverse with `..`, or touch `.git` or `.github`.

### Concurrency, replay and cancellation

- One active turn per page; different pages run up to `PAGERLESS_AGENT_MAX_CONCURRENCY`.
- `409 page_busy`: a different turn is active for the same page.
- `409 turn_running`: the identical request is already active.
- `409 idempotency_conflict`: the turn ID is in use with a different payload.
- `429 capacity`: every turn slot is busy.
- An identical request whose turn finished is replayed from a bounded in-memory cache
  (600 seconds, 256 entries). The cache is not durable: after a restart, Go marks an
  uncertain turn interrupted rather than silently re-running it.
- `POST /turns/{turnId}/stop` returns `{"status":"stopped"|"finished"|"unknown"}`. Once
  stopped, the `/turn` response has `status: "stopped"` and no unverified citations.
- Other errors use `{"error":{"code":"...","message":"..."}}`. Codes include
  `invalid_request`, `unauthorized`, `request_too_large`, `not_found` and `internal_error`.

### Tool callback

The agent sends:

```
POST {callbackUrl}/internal/turns/{turnId}/tools
Authorization: Bearer <internal credential>
{"callId":"c-1","tool":"resources_get","arguments":{"kind":"Node","name":"worker-2"}}
```

The Go service answers:

```json
{"status": "ok", "result": {"data": {"items": []}}, "redacted": false, "truncated": false}
```

`status` is `ok`, `error`, `denied`, `truncated`, `timed_out` or `budget_exhausted`.
Transport retries reuse the same `callId` and payload, so Go can return its recorded result
instead of reading twice. Redirects are refused. A response is capped at 256 KiB.
`budget_exhausted` ends the turn; the other error statuses are returned to the model so it
can explain the missing evidence. A result that echoes the run's integrity nonce is
withheld.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `PAGERLESS_INTERNAL_TOKEN_FILE` | yes | File holding the installer's internal bearer credential. |
| `PAGERLESS_CALLBACK_ORIGIN` | yes | Bare origin of the Go service, e.g. `http://pagerless-server.pagerless.svc:8080`. |
| `PAGERLESS_AGENT_MAX_CONCURRENCY` | no (4) | Global active-turn ceiling, 1–64. |
| `PAGERLESS_AGENT_HOST` | no (`0.0.0.0`) | Listen address; the chart keeps the Service private. |
| `PAGERLESS_AGENT_PORT` | no (8080) | Listen port. |
| `PAGERLESS_AGENT_MAX_REQUEST_BYTES` | no (1 MiB) | Declared request-body ceiling. |
| `PAGERLESS_AGENT_TOOL_TIMEOUT_S` | no (30) | Per-callback timeout. |
| `PAGERLESS_AGENT_READINESS_TTL_S` | no (300) | Successful readiness cache lifetime. |
| `PAGERLESS_AGENT_READINESS_TIMEOUT_S` | no (10) | One readiness probe deadline. |
| `PAGERLESS_AGENT_SHUTDOWN_TIMEOUT_S` | no (10) | Graceful cancellation window. |
| `PAGERLESS_MODEL_BASE_URL` | yes | OpenAI-compatible base URL, e.g. `https://host/v1`. |
| `PAGERLESS_MODEL_NAME` | yes | Tool-calling model identifier. |
| `PAGERLESS_MODEL_API_KEY_FILE` | yes | File holding the model API key. |
| `PAGERLESS_MODEL_HEADERS` | no | JSON object of extra request headers. |
| `PAGERLESS_MODEL_MAX_OUTPUT_TOKENS` | no (4096) | Output cap per call. |
| `PAGERLESS_MODEL_CONTEXT_TOKENS` | no (32768) | Context window used for shedding. |
| `PAGERLESS_MODEL_CONNECT_TIMEOUT_S` | no (10) | Provider connect timeout. |
| `PAGERLESS_MODEL_READ_TIMEOUT_S` | no (90) | Provider read timeout. |
| `PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT` | no | `1` allows an `http` model URL for an in-cluster endpoint. |
| `PAGERLESS_BUDGET_TOOL_CALLS` | no (15) | Default tool-call ceiling. |
| `PAGERLESS_BUDGET_MODEL_CALLS` | no (12) | Default model-call ceiling, including retries. |
| `PAGERLESS_BUDGET_INPUT_TOKENS` | no (300000) | Default input-token ceiling. |
| `PAGERLESS_BUDGET_SECONDS` | no (300) | Default wall-clock ceiling. |

The server refuses to start when a required value is missing or invalid. Errors name the
variable and never print a credential. The internal token and model key are read at
startup; rotate the Secret and restart the pod to pick up a new value.

### Model compatibility

Any endpoint that implements OpenAI Chat Completions with function calling is supported.
The service sends one leading `system` message, `tools`, `tool_choice`, `temperature: 0`,
`max_tokens` (or `max_completion_tokens` after a rejection) and `parallel_tool_calls: false`.
A `400` that names `max_tokens`, `max_completion_tokens`, `temperature`,
`parallel_tool_calls` or `tool_choice` is retried once without that parameter. A `400` naming
`tools` is a clear “tool calls unsupported” failure.

Gemini's OpenAI-compatibility endpoint is the local smoke configuration; it uses the same
code path as any other compatible endpoint:

```sh
PAGERLESS_MODEL_BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai
PAGERLESS_MODEL_NAME=<a tool-calling Gemini model>
PAGERLESS_MODEL_API_KEY_FILE=/path/to/key
```

`GET /readyz` performs one harmless forced tool call, caches the result for
`PAGERLESS_AGENT_READINESS_TTL_S`, and backs off after a failure. It never reports a
credential, URL or provider message in the reason.

## How a turn runs

1. The current operator message directs the work. Earlier messages are sent as a nonce
   envelope of untrusted data, so a follow-up sees the thread without treating it as proof.
2. The model chooses the next read. Tool results are bounded, Markdown-escaped and wrapped
   in the envelope; they are never treated as instructions.
3. `submit_answer` is terminal. Its content, citations and optional proposal are validated
   locally. A forged callId, an unresolvable pointer, an unsafe path or an unsupported
   proposal earns one repair turn; a repeated violation fails the turn visibly.
4. A plain-text answer is accepted as an answer with no citations: this is the explicit
   “missing context” path. It cannot claim evidence.
5. Budgets, the wall clock, provider errors, callback errors and cancellation all produce a
   visible terminal status. The agent never invents a diagnosis and never publishes.

Optional playbooks in `pagerless_agent/playbooks.py` describe diagnostic techniques only.
The model may read one with `read_playbook`; no playbook selects a diagnosis, a resource
kind or an available tool.

## Security boundaries

- Inbound `/turn` and `/stop` require the internal bearer credential, compared in constant
  time.
- The callback destination must match the configured origin; redirects and proxy
  environment settings are ignored.
- No Kubernetes, GitHub, shell or mutation client exists in this package.
- Tool arguments are finite, bounded JSON. Results are bounded, escaped and withheld when
  they probe the envelope nonce.
- Model reasoning is not persisted. Only the terminal content, validated citations,
  proposal and usage returned to Go become part of the transcript.
- Logs name the page, turn, tool and status; they do not contain message bodies, tool
  results or credentials.

## Limitations and dependencies

- The Go service must implement `/internal/turns/{turnId}/tools`, validate citations and
  proposals against its own evidence store, publish any approved proposal, and mark
  interrupted turns after a restart.
- Completed-turn replay is in memory only. There is no database or local transcript.
- The message body is capped by `Content-Length`; this service is reachable only from the
  private Go service, which sends a declared length.
- A model must support function calling. Readiness reports a clear degraded reason when it
  does not.
- Multi-turn state is limited to the `messages` Go sends for each fresh turn.

## Development

```sh
cd agent
uv run --locked pytest
uv run --locked ruff check .
uv run --locked ruff format --check .
uv run --locked mypy src tests
docker build -f ../Dockerfile.pagerless-agent ..
```

The tests use deterministic fake providers and callbacks, checked-in wire fixtures and an
in-process ASGI client. They do not contact a model, a cluster or GitHub.
