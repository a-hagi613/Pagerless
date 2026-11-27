# Pagerless 1.0 release specification

An operator installs Pagerless into a Kubernetes cluster, asks a question in their terminal or browser, follows the evidence, and receives a draft GitHub pull request when a source change is justified. This specification defines the intended release. The implementation on the baseline branch predates it.

## Installation and access

An administrator supplies an OpenAI-compatible Chat Completions endpoint, a model that supports tool calls, a reference to its key, GitHub authorization, and the repository containing the cluster’s source configuration. An optional directory identifies the environment in a shared repository. Examples use placeholders. Installation must work with operator-owned repositories and registries independently of the project maintainer’s accounts.

Helm installs the application, its private Service, read permissions and configured MCP connections. Chart values reference externally provisioned Secrets. The installer must report which required settings are missing. Readiness must distinguish a reachable endpoint from successful model tool calling and authorized repository access. Report the failing prerequisite and how to retry without printing credentials or creating a test PR. Helm prints the actual Service name, namespace and port-forward command. Browser users open the local address. CLI users separately install the kubectl plugin. Helm is the sole installation entry point.

Every authorized operator can see the full installation history and initiate investigations. External access must require operator authentication. The chart defaults to private access. Removing the old login flow requires a tested replacement that covers both interfaces, including in-cluster callers. A port-forward alone does not authorize other network paths.

## Agent and evidence

A **Page** is one investigation conversation. Its first message can be a symptom, an error, or an open question. A namespace and a Kubernetes resource reference are optional hints. A follow-up creates a new turn within the Page. The service assigns stable identifiers to each turn, tool attempt and evidence item.

LangGraph alternates model decisions with bounded tool calls. The standing instructions require checking hypotheses against observed evidence, citing sources, identifying uncertainty and verifying the repository path before drafting a change. Optional playbooks describe diagnostic techniques. Neither failure classification nor a fixed resource-kind list may determine the available diagnoses. The model may finish with an answer, a request for missing context, an inconclusive finding, or a proposed source change.

The service enforces time, concurrency, output-size and model/tool-call limits. Treat workload text, logs, source files and MCP output as untrusted data. Restrict tool capabilities before execution. Sanitize arguments and results before model reuse, event persistence and streaming. Return explicit denied, truncated, timed-out and budget-exhausted outcomes. Report observable actions and evidence. Model-private reasoning is outside the transcript.

## Tools and publication

The Kubernetes MCP server receives a dedicated identity. Permit discovery and approved reads including workloads, events and bounded logs. Exclude Secret objects, exec, attach, proxy, token issuance and mutating verbs. Discovery of a new resource never implies permission to read it. The model process receives the minimum credentials needed for its own role.

Use GitHub MCP for repository reads and a separate publishing path. Confirm the owning source, base commit, allowed repository and path. Generate and validate the diff against the verified base. Create a proposal branch and draft PR with the question, findings, evidence references and expected effect. Never push to the default branch or merge. On an ambiguous source or changed base, explain the problem and revalidate. Recover an existing branch or PR on retry. Questions requiring no source edit get an answer in the conversation.

Future observability MCP servers can supply evidence. A future alert adapter can submit a Page to the same backend as a human. Neither integration is required for 1.0.

## Interfaces and history

Both clients submit the same request, read the same results and resume from the same event cursor. For example, the proposed CLI accepts `kubectl page "Why is checkout timing out?" -n shop`. The dashboard opens to chat, with accessible conversation history, a prompt composer and expandable evidence. Use the existing pager branding, shadcn/ui and Tailwind. Mobile users can inspect calls and diffs without a desktop sidebar.

Persist user messages, terminal answers, citations, sanitized tool attempts and PR references. A trace records tool, start time, status, duration, sanitized arguments, bounded result, and truncation or redaction indicators. Stream updates from an ordered append-only log. Reconnecting from an event ID must neither lose nor duplicate entries. An interrupted turn must have a visible status after restart. Bound retention and document expiration.

## Release acceptance

| Gate | Required evidence |
|---|---|
| Fresh install | Install the packaged chart on a clean kind cluster. Exercise the documented minimum and a current supported Kubernetes version. Verify readiness, invalid credential recovery, browser access and the published CLI. Declare tested versions and platform limitations in release notes. |
| Portability | Use standard Kubernetes APIs and administrator-supplied settings. Test with a deterministic OpenAI-compatible fixture in CI and separately record a real Gemini investigation locally. Tests and builds must run without a maintainer key or cloud account. |
| Independent investigation | Exercise a previously unseen symptom and a question with no failing target. Check cited tool evidence, ambiguous findings, follow-ups and exhaustion. Allow different valid tool sequences. |
| Permissions | Use API authorization checks and negative integration tests for each runtime identity. Deny Secret reads and mutations. Confirm investigator credentials cannot publish and unauthorized clients cannot read history. |
| Trace and UI | Start in CLI and resume in browser, then reverse. Verify failed calls, redaction, reconnect and restart. Run keyboard and narrow/wide viewport checks against the real backend. |
| Pull requests | Test an allowed edit, ambiguous source, stale base, retries and a forbidden destination. Verify one draft against the intended repository and zero live cluster changes. |
| CI and release | Gate Go formatting, lint, tests and race-sensitive paths, Python lint/types/tests, frontend lint/types/tests, browser e2e, Helm lint/render/schema and kind installation. Verify pinned dependencies, release checksums, SBOMs and published artifacts. Publish only under approved fresh coordinates. |

The implementation must document concrete access and credential provisioning, retention defaults, artifact coordinates and compatibility versions before release. Those choices still need review. This specification authorizes no deletion or replacement of published tags or releases.
