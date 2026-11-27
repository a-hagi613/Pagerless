{{/* Common names and labels. */}}
{{- define "pagerless.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pagerless.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "pagerless.labels" -}}
app.kubernetes.io/name: {{ include "pagerless.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "pagerless.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pagerless.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pagerless.serverImage" -}}
{{- $image := .Values.image -}}
{{- if $image.digest -}}
{{- printf "%s@%s" $image.repository $image.digest -}}
{{- else -}}
{{- printf "%s:%s" $image.repository (default .Chart.AppVersion $image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "pagerless.agentImage" -}}
{{- $image := .Values.agentImage -}}
{{- if $image.digest -}}
{{- printf "%s@%s" $image.repository $image.digest -}}
{{- else -}}
{{- printf "%s:%s" $image.repository (default .Chart.AppVersion $image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "pagerless.agentServiceName" -}}
{{- printf "%s-agent" (include "pagerless.fullname" .) -}}
{{- end -}}

{{- define "pagerless.kubernetesMCPServiceName" -}}
{{- printf "%s-kubernetes-mcp" (include "pagerless.fullname" .) -}}
{{- end -}}

{{- define "pagerless.githubMCPServiceName" -}}
{{- printf "%s-github-mcp" (include "pagerless.fullname" .) -}}
{{- end -}}

{{- define "pagerless.prometheusMCPServiceName" -}}
{{- printf "%s-prometheus-mcp" (include "pagerless.fullname" .) -}}
{{- end -}}

{{- define "pagerless.grafanaMCPServiceName" -}}
{{- printf "%s-grafana-mcp" (include "pagerless.fullname" .) -}}
{{- end -}}

{{/* The credentials volume mounts every key of the externally provisioned Secret as a read-only file. */}}
{{- define "pagerless.credentialsVolume" -}}
- name: credentials
  secret:
    secretName: {{ required "existingSecret must name the externally provisioned credentials Secret" .Values.existingSecret }}
    defaultMode: 0400
    items:
      - key: {{ .Values.secretKeys.operatorToken }}
        path: operator-token
      - key: {{ .Values.secretKeys.internalToken }}
        path: internal-token
      - key: {{ .Values.secretKeys.mcpToken }}
        path: mcp-token
      - key: {{ .Values.secretKeys.githubReadToken }}
        path: github-read-token
      - key: {{ .Values.secretKeys.githubWriteToken }}
        path: github-write-token
      - key: {{ .Values.secretKeys.modelApiKey }}
        path: model-api-key
{{- end -}}

{{- define "pagerless.credentialsMount" -}}
- name: credentials
  mountPath: /var/run/pagerless/secret
  readOnly: true
{{- end -}}

{{- define "pagerless.commonEnv" -}}
- name: PAGERLESS_INTERNAL_TOKEN_FILE
  value: /var/run/pagerless/secret/internal-token
{{- end -}}
