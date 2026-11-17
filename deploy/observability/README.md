# Observability stack

Argo CD deploys these components into the `monitoring` namespace. Each Application is defined in [`deploy/apps/templates`](../apps/templates) and reads its Helm values from this directory, at the platform revision.

| Component | Chart | Version | Values |
| --- | --- | --- | --- |
| kube-prometheus-stack (Prometheus, Alertmanager, Grafana, kube-state-metrics) | `prometheus-community/kube-prometheus-stack` | 93.3.0 | [`kube-prometheus-stack/values.yaml`](kube-prometheus-stack/values.yaml) |
| Loki, single-binary mode | `grafana-community/loki` | 21.3.0 | [`loki/values.yaml`](loki/values.yaml) |
| Tempo, single-binary mode | `grafana-community/tempo` | 3.2.1 | [`tempo/values.yaml`](tempo/values.yaml) |
| OpenTelemetry Collector, DaemonSet | `open-telemetry/opentelemetry-collector` | 0.178.3 | [`opentelemetry-collector/values.yaml`](opentelemetry-collector/values.yaml) |

## Container logs

The OpenTelemetry Collector reads `/var/log/pods` on every node, attaches Kubernetes metadata, and exports the records over OTLP to Loki. Loki stores them with resource attributes such as `k8s_namespace_name` and `k8s_pod_name` as labels. Grafana queries them through the Loki data source.

## The OOMKilled alert

[`alerts/oomkilled.yaml`](alerts/oomkilled.yaml) defines the `ContainerOOMKilled` rule. kube-state-metrics reports the pod that was terminated, so the rule joins the pod to its owner. A Deployment is reached through its ReplicaSet, and a StatefulSet directly. Every firing alert has these labels:

- `pagerless_triage: "true"`, which opts the alert in to a Triage (spec #1, Alert Trigger).
- `namespace` and `pod`, and either `deployment` or `statefulset`, which name the Workload.

The Alertmanager route in [`kube-prometheus-stack/values.yaml`](kube-prometheus-stack/values.yaml) sends opted-in alerts to the `pagerless-server` webhook receiver at `http://pagerless-server.pagerless.svc:8080/v1/alerts/alertmanager`. The receiver has an empty `http_config`, where a bearer token is configured when receiver authentication is added.

## Pagerless metrics

`pagerless-server` serves its own metrics on port 9090. [`monitors/pagerless-server.yaml`](monitors/pagerless-server.yaml) is a `ServiceMonitor` that selects the server's Service in the `pagerless` namespace, so Prometheus scrapes it every 30 seconds.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pagerless_triages` | gauge | `phase` | Triages currently in each Phase, collected from the Triage cache at scrape time. |
| `pagerless_proposals_total` | counter | `action` | Validated Proposals, including a Proposal re-validated under a new hash. |
| `pagerless_decisions_total` | counter | `decision` | Human decisions: `Approved` or `Rejected`. |
| `pagerless_verifications_total` | counter | `outcome` | Verification outcomes: `Recovered` or `NotRecovered`. |
| `pagerless_time_to_recovered_seconds` | histogram | — | Seconds from Triage creation to Recovered. |
| `pagerless_triage_input_tokens` | gauge | `triage` | Input tokens the Triage's investigation read, including cached tokens. |
| `pagerless_triage_output_tokens` | gauge | `triage` | Output tokens the Triage's investigation wrote. |
| `pagerless_triage_cost_usd` | gauge | `triage` | Estimated model cost of the Triage in United States dollars. |

The per-Triage series exist only while the Triage resource does, which retention bounds to seven days after a terminal Phase.

## Grafana dashboards

[`dashboards/kustomization.yaml`](dashboards/kustomization.yaml) generates ConfigMaps from [`dashboards/pagerless.json`](dashboards/pagerless.json), [`dashboards/workload-metrics.json`](dashboards/workload-metrics.json) and [`dashboards/workload-logs.json`](dashboards/workload-logs.json), and labels them `grafana_dashboard: "1"`. The kube-prometheus-stack Grafana sidecar discovers that label in every namespace and provisions:

- the **Pagerless** dashboard, which charts every metric above;
- the **Pagerless Workload metrics** dashboard (UID `pagerless-workload`), which charts a Workload's CPU, memory, restarts and ready pods, and which the Triage detail view embeds;
- the **Pagerless Workload logs** dashboard (UID `pagerless-workload-logs`), which shows a Workload's container logs from Loki, and which the Triage detail view embeds.

The Workload dashboards take `namespace` and `workload` variables, which the dashboard API fills in. The Grafana values set `security.allow_embedding: true`, because Grafana refuses to be framed otherwise, and pin the `loki` and `tempo` data source UIDs that the log dashboard references. The Triage detail view always offers a deep link as well, for when embedding is unavailable.

## Local credentials

The Grafana administrator password is generated into the `kube-prometheus-stack-grafana` Secret in the `monitoring` namespace. It exists only in the local cluster, and `make down` deletes it.
