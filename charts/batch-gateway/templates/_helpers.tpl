{{/*
Expand the name of the chart.
*/}}
{{- define "batch-gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "batch-gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "batch-gateway.labels" -}}
helm.sh/chart: {{ include "batch-gateway.chart" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* ========== API Server Helpers ========== */}}

{{/*
API Server fullname
*/}}
{{- define "batch-gateway.apiserver.fullname" -}}
{{- if .Values.apiserver.fullnameOverride }}
{{- .Values.apiserver.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.apiserver.nameOverride }}
{{- if contains $name .Release.Name }}
{{- printf "%s-apiserver" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s-apiserver" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
API Server labels
*/}}
{{- define "batch-gateway.apiserver.labels" -}}
{{ include "batch-gateway.labels" . }}
app.kubernetes.io/name: {{ include "batch-gateway.name" . }}-apiserver
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: apiserver
{{- end }}

{{/*
API Server selector labels
*/}}
{{- define "batch-gateway.apiserver.selectorLabels" -}}
app.kubernetes.io/name: {{ include "batch-gateway.name" . }}-apiserver
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: apiserver
{{- end }}

{{/*
API Server service account name
*/}}
{{- define "batch-gateway.apiserver.serviceAccountName" -}}
{{- if .Values.apiserver.serviceAccount.create }}
{{- default (include "batch-gateway.apiserver.fullname" .) .Values.apiserver.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.apiserver.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
API Server image string
*/}}
{{- define "batch-gateway.apiserver.image" -}}
{{- $tag := .Values.apiserver.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.apiserver.image.repository $tag }}
{{- end }}

{{/* ========== Processor Helpers ========== */}}

{{/*
Processor fullname
*/}}
{{- define "batch-gateway.processor.fullname" -}}
{{- if .Values.processor.fullnameOverride }}
{{- .Values.processor.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.processor.nameOverride }}
{{- if contains $name .Release.Name }}
{{- printf "%s-processor" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s-processor" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Processor labels
*/}}
{{- define "batch-gateway.processor.labels" -}}
{{ include "batch-gateway.labels" . }}
app.kubernetes.io/name: {{ include "batch-gateway.name" . }}-processor
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: processor
{{- end }}

{{/*
Processor selector labels
*/}}
{{- define "batch-gateway.processor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "batch-gateway.name" . }}-processor
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: processor
{{- end }}

{{/*
Processor service account name
*/}}
{{- define "batch-gateway.processor.serviceAccountName" -}}
{{- if .Values.processor.serviceAccount.create }}
{{- default (include "batch-gateway.processor.fullname" .) .Values.processor.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.processor.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Processor image string
*/}}
{{- define "batch-gateway.processor.image" -}}
{{- $tag := .Values.processor.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.processor.image.repository $tag }}
{{- end }}
