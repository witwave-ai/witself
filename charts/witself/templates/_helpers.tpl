{{/*
Expand the name of the chart.
*/}}
{{- define "witself.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this
(by the DNS naming spec).
*/}}
{{- define "witself.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "witself.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "witself.labels" -}}
helm.sh/chart: {{ include "witself.chart" . }}
{{ include "witself.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: witself
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "witself.selectorLabels" -}}
app.kubernetes.io/name: {{ include "witself.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the service account to use.
*/}}
{{- define "witself.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "witself.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Resolve the server image reference, defaulting the tag to the chart appVersion.
*/}}
{{- define "witself.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
Whether KMS configuration applies. KMS is consumed only when the sealed plane
is enabled. (local-dev still sets the provider env, but needs no external KMS.)
*/}}
{{- define "witself.kmsEnabled" -}}
{{- .Values.sealedPlane.enabled -}}
{{- end }}

{{/*
Whether the active embedding provider requires outbound egress / an API key.
local-dev requires neither.
*/}}
{{- define "witself.embeddingsExternal" -}}
{{- ne .Values.embeddings.provider "local-dev" -}}
{{- end }}

{{/*
Whether the active KMS provider requires outbound egress / credentials.
True only when the sealed plane is enabled AND the provider is not local-dev.
*/}}
{{- define "witself.kmsExternal" -}}
{{- and .Values.sealedPlane.enabled (ne .Values.kms.provider "local-dev") -}}
{{- end }}

{{/*
Secret-backed environment variables. Pulled from existing Secrets only; the
chart never embeds raw secret material.
*/}}
{{- define "witself.secretEnv" -}}
- name: WITSELF_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret.name | quote }}
      key: {{ .Values.database.existingSecret.urlKey | quote }}
{{- if and (include "witself.embeddingsExternal" .) .Values.embeddings.existingSecret .Values.embeddings.existingSecret.name }}
- name: WITSELF_EMBEDDINGS_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.embeddings.existingSecret.name | quote }}
      key: {{ .Values.embeddings.existingSecret.apiKeyKey | quote }}
{{- end }}
{{- end }}

{{/*
envFrom sources for credentials injected wholesale (KMS, blob, extra).
*/}}
{{- define "witself.envFrom" -}}
{{- if and (eq (include "witself.kmsExternal" .) "true") .Values.kms.existingSecret .Values.kms.existingSecret.envFrom .Values.kms.existingSecret.name }}
- secretRef:
    name: {{ .Values.kms.existingSecret.name | quote }}
{{- end }}
{{- if and .Values.blob.enabled .Values.blob.existingSecret .Values.blob.existingSecret.envFrom .Values.blob.existingSecret.name }}
- secretRef:
    name: {{ .Values.blob.existingSecret.name | quote }}
{{- end }}
{{- with .Values.server.extraEnvFrom }}
{{- toYaml . }}
{{- end }}
{{- end }}
