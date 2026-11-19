{{/*
Chart-wide helpers. Every namespaced object uses a fixed name because the
software, the CLI, and the documented RBAC checks refer to each other by name;
only the namespace is a release parameter. Cluster-scoped objects carry the
release name so two releases cannot fight over one ClusterRole.
*/}}

{{/* Chart name and version, used in the standard labels. */}}
{{- define "pagerless.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard labels for one component. Call with a dict:
  include "pagerless.labels" (dict "context" $ "name" "pagerless-server")
*/}}
{{- define "pagerless.labels" -}}
helm.sh/chart: {{ include "pagerless.chart" .context }}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
app.kubernetes.io/version: {{ .context.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .context.Release.Service }}
{{- end -}}

{{/*
Fully qualified image reference. An empty tag uses the chart's appVersion, which
matches the image tags that CI publishes for the same release.
Call with a dict: include "pagerless.image" (dict "context" $ "image" .Values.images.server)
*/}}
{{- define "pagerless.image" -}}
{{- printf "%s:%s" .image.repository (default .context.Chart.AppVersion .image.tag) -}}
{{- end -}}

{{/*
The namespaced read rules, shared by the cluster-wide read ClusterRole and every
allowlisted read Role. Call with the root context.
*/}}
{{- define "pagerless.namespacedReadRules" -}}
- apiGroups: [""]
  resources: [pods, services, endpoints, configmaps, persistentvolumeclaims]
  verbs: [get, list, watch]
# The log subresource is separate in RBAC; get alone tails a container log and
# grants no exec, attach, or port-forward.
- apiGroups: [""]
  resources: [pods/log]
  verbs: [get]
# Both Event APIs are read: core/v1 producers are still common, and
# events.k8s.io/v1 is the durable API.
- apiGroups: [""]
  resources: [events]
  verbs: [get, list, watch]
- apiGroups: [events.k8s.io]
  resources: [events]
  verbs: [get, list, watch]
- apiGroups: [apps]
  resources: [deployments, replicasets, statefulsets, daemonsets]
  verbs: [get, list, watch]
- apiGroups: [batch]
  resources: [jobs, cronjobs]
  verbs: [get, list, watch]
- apiGroups: [discovery.k8s.io]
  resources: [endpointslices]
  verbs: [get, list, watch]
- apiGroups: [networking.k8s.io]
  resources: [ingresses]
  verbs: [get, list, watch]
{{- end -}}

{{/*
The cluster-scoped read rules. They cannot be rendered as a namespaced Role, so
allowlist mode drops them.
*/}}
{{- define "pagerless.clusterReadRules" -}}
- apiGroups: [""]
  resources: [nodes, namespaces]
  verbs: [get, list, watch]
# Structural validation of a proposed manifest without a cluster write.
- nonResourceURLs: ["/openapi/v3", "/openapi/v3/*"]
  verbs: [get]
# Authenticates the agent Job's projected token on every report and tool call.
# It is an authentication request, not a write to any object.
- apiGroups: [authentication.k8s.io]
  resources: [tokenreviews]
  verbs: [create]
{{- end -}}

{{/*
Refuses a watch configuration that cannot be honoured. Cluster-wide watching and
an allowlist are mutually exclusive, and an allowlist with no entries would
render no read permission at all.
*/}}
{{- define "pagerless.validateWatch" -}}
{{- if .Values.rbac.watchAllNamespaces -}}
{{- if gt (len .Values.rbac.watchNamespaces) 0 -}}
{{- fail "rbac.watchAllNamespaces and rbac.watchNamespaces are mutually exclusive: set rbac.watchAllNamespaces=false to use an allowlist" -}}
{{- end -}}
{{- else if eq (len .Values.rbac.watchNamespaces) 0 -}}
{{- fail "rbac.watchNamespaces must name at least one namespace when rbac.watchAllNamespaces is false" -}}
{{- end -}}
{{- end -}}

{{/*
Comma-separated allowlist the server is given, or an empty string in
cluster-wide mode. A `*` is deliberately not passed: the server rejects it as an
invalid namespace name, and an empty list already means "no allowlist".
*/}}
{{- define "pagerless.allowlist" -}}
{{- if not .Values.rbac.watchAllNamespaces -}}
{{- join "," .Values.rbac.watchNamespaces -}}
{{- end -}}
{{- end -}}

{{/*
The credentials Secret the Deployment mounts: an externally managed one, or the
one the chart creates.
*/}}
{{- define "pagerless.credentialsSecret" -}}
{{- default .Values.credentials.secretName .Values.credentials.existingSecret -}}
{{- end -}}
