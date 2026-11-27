# Rewrite audit

The [file inventory](rewrite-inventory.csv) accounts for every path tracked in baseline commit `753d590`, including fixtures, generated files, lockfiles, artwork and workflows. Filter the disposition column to review a deletion batch. The baseline commit retains the original content. The private AGENTS.md and local planning tickets were outside that commit. The ignore rule keeps workstation instructions out of the public repository and Docker context.

## Reading the inventory

**Remove now** means obsolete documentation or portal images removed in the specification change. **Update now** identifies the public landing page and private-file exclusions. **Retire** means delete the file and its obsolete behavior in the indicated subsystem cutover. **Replace** means discard the implementation while preserving a required product responsibility. **Adapt** identifies reusable code to edit in place. **Keep** preserves an independent component. **Regenerate** applies to dependency lockfiles after their manifests settle.

## Cutover order

1. **Introduce the shared conversation API and storage.** Create an investigation, add a turn, read history and stream ordered events after a cursor. Replace the Go composition root and Python Job runtime together. Remove Page types, controllers and CRDs after both clients use the new API.
2. **Switch tool execution and publication.** Replace the broker and legacy GitHub client with scoped MCP adapters. Retain redaction, citation validation and bounded diff generation. Remove direct AWS, Grafana, Datadog and other provider connectors, watcher, cooldown, alert intake, fixed scaling/rollback recipes, sandbox and post-merge checks with their registration and consumers.
3. **Replace the CLI and portal.** Ship the prompt-first command and chat view. Retire the old approve/reject, config, login and resource-management flows. Replace authentication at the same time as the protected endpoints. Reuse the UI primitives, theme, diff parser and static asset server. Replace content-derived trace keys with stable event IDs.
4. **Cut over Helm and release automation.** Remove old CRDs, Jobs, writable credential management, old image coordinates and stale install notes. Prune unused modules and regenerate lockfiles only after importers are gone. Keep the current checks running until equivalent new tests cover each replaced entry point. Validate the package in kind before advertising an install command.
5. **Retire public distribution separately.** Inventory tags, GitHub releases, registry charts and images, Krew and Homebrew references. Select fresh coordinates and get owner confirmation on the specific retirement actions. Preserve license, attribution and source history.

## Planned additions

These proposed paths show where implementation work would land. They do not claim that these components exist today.

| New path or package | Responsibility and first verification |
|---|---|
| `api/investigations/openapi.yaml` | Request, turn, event, error and cursor schemas. Contract tests shared by CLI and browser. |
| `internal/investigations/` | Investigation service and operator access checks. Reject unauthorized reads and duplicate submissions. |
| `internal/transcript/` | Durable event sequence, evidence, replay and retention. Test restart and concurrent subscribers. |
| `internal/mcp/` | Versioned MCP connections, capability filtering, deadlines and bounded/redacted results. Test denied tools and malformed responses. |
| `internal/githubpublish/` | Isolate branch and PR writes. Verify base revision, source ownership and retry recovery. |
| `agent/src/pagerless_agent/playbooks/` | Optional diagnostic guidance. Test open-ended questions outside the fixtures. |
| `dashboard/src/pages/chat.tsx` and `dashboard/src/components/conversation/` | Composer, history, citations and trace details. Test keyboard navigation, mobile and client parity. |
| `internal/cli/chat.go` and `internal/cli/trace.go` | Natural-language requests and replay using the same API as the dashboard. |
| `charts/pagerless/templates/kubernetes-mcp.yaml` and `github-mcp.yaml` | Install or configure MCP integrations with distinct credentials and constrained access. Test rendered grants and actual tool calls. |
| `test/e2e/investigation/` | Real kind, packaged chart and both clients. Use reproducible model and GitHub fixtures in PR CI. |

## Why the largest deletions belong together

The old CLI writes Page resources. The controller starts Jobs from those resources, and the dashboard reads status from them. Deleting one package while callers still compile against it breaks the baseline and obscures which behavior has a replacement. Replace a complete request path, prove it, then remove those imports and the old tests in the same change.

The new stored transcript must record retries individually. The old evidence store overwrites records sharing a Page and call ID. The UI also merges steps by time, tool and summary. Retaining either behavior would hide distinct attempts that the operator explicitly needs to see.

The existing release workflow contains retry logic that can remove a previous GitHub release. Replace it before publishing the new distribution. The current release channels and personal tap identifiers are facts about legacy packaging, not prerequisites for a user’s cluster or GitHub repository.
