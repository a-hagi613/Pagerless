# Connectors and alert webhooks

A Connector is a configured, read-only integration that supplies evidence or
Triggers. The chart renders the non-secret half of the configuration into the
`pagerless-connectors` ConfigMap; credentials stay in the `pagerless-credentials`
Secret and are referenced by name and key (ADR-0040).

## Native logs are always on

Native Kubernetes log collection is the only evidence source every cluster has,
so it cannot be disabled. The chart fails to render when
`connectors.logs.nativeKubernetes.enabled` is false. Its bounds are tail line
count, byte ceiling, look-back window, Pods per bundle, containers per Pod, and
concurrency. The collector reads the pod log subresource, requests the previous
container's log after a restart, and falls back to the current log when the
previous one is empty.

## Optional Connectors

Enable only the backends the cluster already runs. Every entry takes a URL, a
query template, bounds, and an `auth` reference to the credentials Secret.

| Connector | Key | Queries |
| --- | --- | --- |
| Loki | `logs.loki` | logs |
| Grafana | `logs.grafana`, `metrics.grafana` | logs, metrics through the datasource proxy or unified query API |
| CloudWatch | `logs.cloudwatchLogs`, `metrics.cloudwatch` | logs and metrics |
| Datadog | `logs.datadogLogs`, `metrics.datadog` | logs and metrics |
| OpenSearch | `logs.opensearch` | logs |
| Prometheus | `metrics.prometheus` | metrics |
| Alertmanager | `alertState.alertmanager` | firing alerts |
| Grafana alerting | `alertState.grafana` | firing alerts |
| MCP | `logs.mcp`, `metrics.mcp` | one read-only tool per Connector |

An MCP Connector invokes exactly the tool named in its configuration; the tool
name never comes from model output.

## Authentication

Each sender authenticates independently, and an unconfigured dialect refuses
every delivery rather than accepting one anonymously.

| Sender | Credential | Scheme |
| --- | --- | --- |
| Alertmanager | `ALERTMANAGER_WEBHOOK_TOKEN` | Bearer token |
| Grafana | `GRAFANA_WEBHOOK_SECRET` | HMAC-SHA256 of the exact body in `X-Pagerless-Signature` |
| Datadog | `DATADOG_WEBHOOK_TOKEN` | Bearer token |
| Generic | `GENERIC_WEBHOOK_TOKEN` | Bearer token |
| SNS and CloudWatch | `connectors.alerting.snsTopics` and `verifySNSSignature` | Topic allowlist and optional signature |

The receiver never confirms an SNS subscription. Confirm it deliberately in the
AWS console, then add its ARN to `connectors.alerting.snsTopics`.

The four webhook tokens are not part of the Portal settings route or
`kubectl page config`: they are not AI or repository credentials. Write them to
the credentials Secret with your secret manager, or with `kubectl` when
Pagerless owns the Secret, before you enable a dialect.

## Joining a Page

An alert must carry `pagerless.dev/watch: "true"` (or the v1 `pagerless_triage`
label) and name its subject with a supported label, the
`pagerless.dev/subject` annotation, or a configured extraction rule. The v1
spelling is a label value, not a retired resource name: it survives migration by
design, because dropping it would silently stop alerts for an existing
installation, and `IsOptedIn` accepts both spellings. An alert whose subject
cannot be resolved is ignored with a reason and reported to the sender with a
`202` response.

Datadog requires the payload template shipped at
`charts/pagerless/files/datadog-webhook-template.json` (and kept in sync with
`DatadogPayloadTemplate` in `internal/webhooks/decode.go`) so the receiver sees
the alert identity, status, severity, timing, and tags.

## Bundle storage

The Context Bundle is assembled once per Page, in a fixed order, with a per
component cap in 2 MiB serialized total and a 25-second wall-clock budget, then
sanitized and stored in the history database with its hash, byte count, and
truncation flag. A Connector that is unreachable, unauthorized, or rate-limited
adds a degradation note that names it; the Page proceeds on native logs alone.
