{{/* Expand the name of the chart. */}}
{{- define "witself-server.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "witself-server.fullname" -}}
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

{{- define "witself-server.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "witself-server.labels" -}}
helm.sh/chart: {{ include "witself-server.chart" . }}
{{ include "witself-server.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: witself
{{- end -}}

{{- define "witself-server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "witself-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The worker intentionally has a different app name as well as a component
label. The existing server Deployment and Service selectors predate component
labels and are immutable/disruptive to change in one rollout; the distinct app
name prevents either selector set from ever matching the other workload.
*/}}
{{- define "witself-server.workerName" -}}
witself-worker
{{- end -}}

{{- define "witself-server.workerFullname" -}}
{{- $serverFullname := include "witself-server.fullname" . -}}
{{- if hasSuffix "-server" $serverFullname -}}
{{- printf "%s-worker" (trimSuffix "-server" $serverFullname) | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-worker" ($serverFullname | trunc 56 | trimSuffix "-") -}}
{{- end -}}
{{- end -}}

{{- define "witself-server.workerMetricsFullname" -}}
{{- $workerBase := include "witself-server.workerFullname" . | trimSuffix "-worker" -}}
{{- printf "%s-worker-metrics" ($workerBase | trunc 48 | trimSuffix "-") -}}
{{- end -}}

{{- define "witself-server.workerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "witself-server.workerName" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: worker
{{- end -}}

{{- define "witself-server.workerLabels" -}}
helm.sh/chart: {{ include "witself-server.chart" . }}
{{ include "witself-server.workerSelectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: witself
{{- end -}}

{{- define "witself-server.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "witself-server.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The image reference, defaulting the tag to the chart appVersion. */}}
{{- define "witself-server.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
