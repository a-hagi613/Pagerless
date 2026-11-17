# Pagerless

Guidance for AI coding agents and contributors working in this repository. Read [`README.md`](README.md) for the system overview, [`GLOSSARY.md`](GLOSSARY.md) for vocabulary, and [`docs/adr/`](docs/adr/README.md) for the decisions that constrain the design.

## Quality bar

This repository is a public reference project and is reviewed closely. Every artifact (code, documentation, ADRs, commit messages, issues, and pull requests) must meet a production standard.

- Write in clear, precise, professional English. Avoid informal phrasing and filler.
- Do not commit placeholders, TODO stubs, commented-out code, or unverifiable claims.
- Document only what exists or is explicitly marked as planned.
- Pin every dependency, image, chart, and tool to an exact version, verified against current releases at the time it is added.
- Use the vocabulary in `GLOSSARY.md` exactly, in prose and in identifiers.
- Record decisions that are hard to reverse, surprising, or the result of a real trade-off as ADRs, using the format of the existing records.
- Write commit messages in the imperative mood, with a concise subject line and a body that explains why.

## Planned repository layout

```
api/         Triage and TriageApproval CRD types (kubebuilder)
cmd/         pagerless-server, pagerless-executor, kubectl-triage
internal/    Go packages
agent/       Python LangGraph agent (uv)
dashboard/   React dashboard, served by pagerless-server
deploy/      Argo CD app of apps, platform manifests, Helm values
infra/       Terraform for EKS
evals/       pytest harness, promptfoo suites, Replay recordings
docs/        ADRs and architecture documentation
```

Demo Workloads live in the separate `pagerless-sandbox` repository (ADR-0003).

## Development environment

Development runs in WSL2 (Ubuntu), with the repository cloned into the Linux filesystem rather than under `/mnt/c`. The full local stack requires roughly 24 GB of memory allocated to WSL.

## Development workflow

Claude Opus 5.5 orchestrates: it plans, routes work, and reviews. It does not implement tickets itself.

Implementation is performed by OpenCode with `opencode-go/deepseek-v4.1-flash`, using parallel sub-agents, one ticket each. Free models are never used.

### Conserving tokens

- Keep tickets small and self-contained, and pass context pointers (spec, ticket, notes, commits) rather than restating content.
- Run independent tickets in parallel in background sub-agents.
- Keep sub-agent reports to a few lines.

## Agent skills

### Issue tracker

Issues and specs are tracked in GitHub Issues on `a-hagi613/pagerless` using the `gh` CLI. See [`docs/agents/issue-tracker.md`](docs/agents/issue-tracker.md).

### Triage labels

The five default issue triage labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See [`docs/agents/triage-labels.md`](docs/agents/triage-labels.md).

### Domain docs

Single context: one `GLOSSARY.md` and `docs/adr/` at the repository root. See [`docs/agents/domain.md`](docs/agents/domain.md).
