{{/*
Expand the name of the chart.
*/}}
{{- define "fold.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name, truncated at 63 chars (DNS naming).
*/}}
{{- define "fold.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "fold.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "fold.labels" -}}
helm.sh/chart: {{ include "fold.chart" . }}
{{ include "fold.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "fold.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fold.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "fold.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "fold.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the ConfigMap holding fold.config.json. Exactly one of .Values.config
(inline) and .Values.existingConfigMap must be set.
*/}}
{{- define "fold.configMapName" -}}
{{- if and .Values.existingConfigMap .Values.config }}
{{- fail "set only one of `config` (inline) and `existingConfigMap`" }}
{{- else if .Values.existingConfigMap }}
{{- .Values.existingConfigMap }}
{{- else if .Values.config }}
{{- include "fold.fullname" . }}
{{- else }}
{{- fail "a gateway config is required: set `config` (inline JSON document) or `existingConfigMap` (name of a ConfigMap with key fold.config.json)" }}
{{- end }}
{{- end }}

{{/*
Host header for httpGet probes. The gateway's DNS-rebinding protection
(server.allowedHosts) rejects requests for hostnames outside the allowlist —
including kubelet probes, which send Host: <podIP>:<port>. Port is stripped
before matching, so any allowlisted hostname works.

Resolution: probes.hostHeader if set; else the first non-"*" entry of the
inline config's server.allowedHosts; else "localhost", which the gateway's
default allowlist (used when allowedHosts is unset) admits. With
existingConfigMap the chart cannot see the allowlist, so hostHeader is
required.
*/}}
{{- define "fold.probeHostHeader" -}}
{{- if .Values.probes.hostHeader }}
{{- .Values.probes.hostHeader }}
{{- else if .Values.existingConfigMap }}
{{- fail "probes.hostHeader is required with existingConfigMap: set it to a hostname in your config's server.allowedHosts, or probes will be rejected 403 and pods will never become ready" }}
{{- else }}
{{- $hosts := dig "server" "allowedHosts" (list) .Values.config }}
{{- if and $hosts (ne (first $hosts | toString) "*") }}
{{- first $hosts }}
{{- else }}
{{- "localhost" }}
{{- end }}
{{- end }}
{{- end }}
