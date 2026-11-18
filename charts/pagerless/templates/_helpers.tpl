{{/*
Chart-wide helpers. Every resource name is fixed because the software and its
admission policies refer to each other by name; only the namespace is a release
parameter.
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

{{/* Comma-separated Workload namespaces, as pagerless-server expects them. */}}
{{- define "pagerless.workloadNamespaces" -}}
{{- required "workloadNamespaces must contain at least one namespace" .Values.workloadNamespaces | join "," -}}
{{- end -}}

{{/*
Comma-separated MCP servers the agent may call. k8s and grafana are always
present; argocd and github follow the integrations that are configured. The
explicit mcp.servers value overrides the derivation, as --mcp-servers does in
hack/install.sh.
*/}}
{{- define "pagerless.mcpServers" -}}
{{- if .Values.mcp.servers -}}
{{- .Values.mcp.servers -}}
{{- else -}}
{{- $servers := list "k8s" "grafana" -}}
{{- if .Values.argocd.application -}}
{{- $servers = append $servers "argocd" -}}
{{- end -}}
{{- if .Values.sandbox.url -}}
{{- $servers = append $servers "github" -}}
{{- end -}}
{{- join "," $servers -}}
{{- end -}}
{{- end -}}

{{/*
Owner/name slug of sandbox.url, or an empty string when the URL is not a
GitHub URL with exactly two path segments. A trailing slash and a .git suffix
are tolerated, as in hack/install.sh.
*/}}
{{- define "pagerless.sandboxSlug" -}}
{{- $url := .Values.sandbox.url | trimSuffix "/" | trimSuffix ".git" -}}
{{- if hasPrefix "https://github.com/" $url -}}
{{- $slug := trimPrefix "https://github.com/" $url -}}
{{- if regexMatch "^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$" $slug -}}
{{- $slug -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Sandbox owner, explicitly configured or parsed from sandbox.url. */}}
{{- define "pagerless.sandboxOwner" -}}
{{- if .Values.sandbox.owner -}}
{{- .Values.sandbox.owner -}}
{{- else -}}
{{- $parts := splitList "/" (include "pagerless.sandboxSlug" .) -}}
{{- if eq (len $parts) 2 -}}
{{- index $parts 0 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Sandbox repository, explicitly configured or parsed from sandbox.url. */}}
{{- define "pagerless.sandboxRepo" -}}
{{- if .Values.sandbox.repo -}}
{{- .Values.sandbox.repo -}}
{{- else -}}
{{- $parts := splitList "/" (include "pagerless.sandboxSlug" .) -}}
{{- if eq (len $parts) 2 -}}
{{- index $parts 1 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Keycloak token endpoint. The agent runtime requires it to be exactly the
issuer's token endpoint, so an explicit override must describe the same realm.
*/}}
{{- define "pagerless.oidcTokenUrl" -}}
{{- if .Values.oidc.tokenUrl -}}
{{- .Values.oidc.tokenUrl -}}
{{- else -}}
{{- printf "%s/protocol/openid-connect/token" .Values.oidc.issuer -}}
{{- end -}}
{{- end -}}

{{/*
Admission policy identity of pagerless-server. The dry-run policy and the agent
Job policy match this exact username, so a different release namespace changes
both the ServiceAccount and the policy expression together.
*/}}
{{- define "pagerless.serverUsername" -}}
{{- printf "system:serviceaccount:%s:pagerless-server" .Release.Namespace -}}
{{- end -}}
