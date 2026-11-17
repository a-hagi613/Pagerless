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

## Local credentials

The Grafana administrator password is generated into the `kube-prometheus-stack-grafana` Secret in the `monitoring` namespace. It exists only in the local cluster, and `make down` deletes it.
