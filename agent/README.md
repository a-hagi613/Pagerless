# Investigation agent

`python -m pagerless_agent` runs one Page investigation as an ephemeral, zero-RBAC Job.
The agent reads a Context Bundle and a scoped tool catalogue from `pagerless-server`,
investigates with read-only tools, and submits one report: a cited Diagnosis and exactly
one Proposal, either a `FilePatch` against the Target Repository or a `DiagnosisOnly`
conclusion. It never writes to a cluster, a connector, or GitHub.

The Job receives its Page, Target, and projected report-token settings through the
environment defined in `internal/agentapi/contract.go`. The agent needs no Kubernetes
client, no connector credential, and no GitHub token. Its only credentials are the
Page-scoped projected service-account token and the model API key, and its only two
network destinations are `PAGERLESS_SERVER_URL` and `PAGERLESS_MODEL_BASE_URL`.

## One client for every endpoint

The model client speaks only the OpenAI Chat Completions protocol, through the pinned
`openai` SDK, configured with a base URL, an API key, a model name, and optional default
headers. That reaches OpenAI, Azure, DeepSeek, OpenRouter, Gemini's compatibility
endpoint, and a vLLM or Ollama instance on a laptop without a provider-specific code path.
SDK retries are disabled (`max_retries=0`) so every model call is one the budget accounts
for.

Requests are normalised for the whole ecosystem:

- exactly one `system` message, first, and never `developer`;
- provider-private reasoning fields are dropped rather than echoed back;
- an empty tool result is sent as `{"ok": true, "data": null}`;
- tool arguments are repaired (empty, double-encoded, fenced, trailing commas) before the
  graph sees them, and an argument that cannot be repaired becomes a model-visible tool
  error rather than a failed Job.

## The structured-output ladder

No provider is assumed to support strict schemas, JSON mode, or function calling. A stage
that needs structured output negotiates four rungs, strongest first:

| Rung | Request | Reached when |
| --- | --- | --- |
| `json_schema` | `response_format` with a strict schema projection | The endpoint accepts it |
| `json_object` | `response_format: json_object` and the schema in the prompt | The endpoint rejects `json_schema` |
| `tool` | one forced call to `submit_output` | The endpoint rejects `response_format` |
| `text` | JSON extracted from the answer | The endpoint rejects a forced `tool_choice` or offers no function calling |

A `400` that names the rejected parameter descends one rung within the same model-call
slot. The rung is cached per `(base_url, model, stage)` and reported in the Page's
`capabilities.structuredOutput`, so an operator can see an installation running degraded.
Local Pydantic validation with `extra="forbid"` is authoritative at every rung, because
the strict projection drops the path and pattern constraints the contract relies on. A
validation failure earns exactly one repair turn, and a second failure fails the stage.

## The tool broker

One call starts the investigation:

```
GET /v1/pages/{page}/investigation
```

It returns the Context Bundle, the tool catalogue, the enforced scope, and the limits.
Tool calls then go to `POST /v1/pages/{page}/tools/{tool}` with a Page UID header and an
`Idempotency-Key` of `<page-uid>/<call-id>`, and the final report to
`POST /v1/pages/{page}/report`. There is no write tool in the catalogue; a model that asks
for one receives a normal tool error.

Every tool result reaches the model inside a `<tool_output nonce="…" source="…">`
envelope whose nonce is 128 bits per run, with the payload JSON-encoded and Markdown
structure markers escaped. A log line can therefore only ever be a string value. A result
that echoes the run nonce, and a second call to a tool outside the catalogue, raise a
`SuspiciousOutput` event on the Page; the run continues read-only.

## Budgets

| Budget | Default | Enforced with |
| --- | --- | --- |
| Tool calls | 15 | Counted before the broker request |
| Model calls | 12 | Counted before every HTTP attempt, including retries |
| Input tokens | 300 000 | A pessimistic pre-flight estimate, then the provider's usage |
| Wall clock | 300 s | `asyncio.timeout` around every await |

Context pressure sheds the oldest tool results first, then truncates the rest, and never
drops the system prompt, the task message, or the last assistant turn. A provider that
omits `usage` is estimated pessimistically and the report marks the figure `estimated`, so
the Page never presents an estimate as fact. Any exhausted budget ends the Page as
`BudgetExhausted` and keeps the Findings gathered so far.

## Configuration

The server owns the Job's environment. The agent never reads a ConfigMap or a Secret by
name.

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `PAGERLESS_TASK` | no | `investigate` | `investigate` or `narrative` |
| `PAGERLESS_SERVER_URL` | yes | — | Base URL of the investigation API |
| `PAGERLESS_TOKEN_FILE` | yes | — | Projected token for `pagerless-server/page/<uid>` |
| `PAGERLESS_PAGE` | yes | — | Page name |
| `PAGERLESS_PAGE_UID` | yes | — | Page UID, echoed in every request |
| `PAGERLESS_MODEL_BASE_URL` | yes | — | e.g. `https://api.deepseek.com/v1` |
| `PAGERLESS_MODEL_NAME` | yes | — | The provider's model identifier |
| `PAGERLESS_MODEL_API_KEY_FILE` | yes | — | File holding the key |
| `PAGERLESS_MODEL_HEADERS` | no | `{}` | JSON object of extra request headers |
| `PAGERLESS_MODEL_MAX_OUTPUT_TOKENS` | no | `4096` | Per-call output cap |
| `PAGERLESS_MODEL_CONTEXT_TOKENS` | no | `32768` | Window used for context shedding |
| `PAGERLESS_MODEL_STRUCTURED_OUTPUT` | no | `auto` | `auto`, `json_schema`, `json_object`, `tool`, `text` |
| `PAGERLESS_MODEL_MAX_TOKENS_FIELD` | no | `auto` | `max_tokens`, `max_completion_tokens`, `auto` |
| `PAGERLESS_MODEL_SUPPORTS_TEMPERATURE` | no | `auto` | `auto`, `true`, `false` |
| `PAGERLESS_MODEL_CONNECT_TIMEOUT_S` | no | `10` | Connection timeout |
| `PAGERLESS_MODEL_READ_TIMEOUT_S` | no | `90` | Read timeout |
| `PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT` | no | unset | `1` permits an `http` model endpoint for in-cluster vLLM or Ollama |
| `PAGERLESS_BUDGET_TOOL_CALLS` | no | `15` | Investigation budget |
| `PAGERLESS_BUDGET_MODEL_CALLS` | no | `12` | Investigation budget |
| `PAGERLESS_BUDGET_INPUT_TOKENS` | no | `300000` | Investigation budget |
| `PAGERLESS_BUDGET_SECONDS` | no | `300` | Investigation budget |
| `PAGERLESS_HINT` | no | — | Human hint, read as untrusted data |

The investigation manifest carries the limits the server enforces, and those take
precedence over the Job's declared values.

## Outcomes

| Outcome | Exit | Effect |
| --- | --- | --- |
| `reported` | 0 | The report was accepted; the Page moves to `Proposed` |
| `failed` | 1 | The agent failed; the Findings already posted stay |
| `budget_exhausted` | 2 | A budget ran out; the Findings stay |
| `proposal_refused` | 3 | The Proposal was refused after one revision |
| `rejected` | 4 | The server refused the Job |
| `provider_auth_failed` | 5 | The model endpoint refused the credentials |
| `provider_rejected` | 6 | The model endpoint rejected the request |

## Narrative

`PAGERLESS_TASK=narrative` runs the second, short-lived Job. It reads the Page record,
makes exactly one model call capped at 2,000 output tokens, and posts the Narrative back.
It has no tools and no Context Bundle, and it never writes a phase, a check, or a Finding.

## Tests

```sh
uv sync --locked
uv run --locked pytest
uv run --locked ruff check .
uv run --locked ruff format --check .
uv run --locked mypy src tests
```

The suite is entirely offline. `tests/mock_openai.py` is a fixture-backed
OpenAI-compatible server reached through `httpx2.MockTransport`: it serves the scripted
investigation in `tests/fixtures/scripts/`, reproduces the four ladder profiles by
rejecting the parameters each class of provider rejects, and injects the faults real
endpoints produce (`400` on `response_format`, `400` on `tool_choice`, malformed
arguments, truncation, `429`, and a response with no `usage`). No test needs a network, a
model credential, or a GitHub credential.
