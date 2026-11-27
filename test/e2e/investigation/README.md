# Integration fixture

`fake` is the deterministic integration server used by `hack/verify-chart.sh`. It is built into the
`pagerless-e2e:e2e` image and never shipped in a release.

| Port | Surface |
| --- | --- |
| 9090 | OpenAI-compatible Chat Completions model |
| 9091 | GitHub REST repository, branches, commits and draft pull requests |
| 9092 | Read-only Prometheus API (`query`, `alerts`, `targets`) |
| 9093 | Read-only Grafana API (unified alerts, dashboard search/detail, data sources) |

The Prometheus alerts and Grafana dashboards mirror the observability stack in
[`pagerless-sandbox`](https://github.com/a-hagi613/pagerless-sandbox):

- `ContainerOOMKilled`, `ServiceErrorRateHigh` and `ContainerMemoryGrowth` carry the namespace and
  deployment labels from `platform/observability/alerts`.
- `pagerless-target` and `pagerless-target-logs` are the dashboard UIDs from
  `platform/observability/dashboards`.
- The data source list includes the credential fields the real API returns, so the Grafana MCP
  provider's redaction is exercised.

`hack/verify-chart.sh` enables both observability integrations against these endpoints and asserts
that `prometheus_mcp` and `grafana_mcp` are admitted readiness checks. The MCP providers never write
to either API.
