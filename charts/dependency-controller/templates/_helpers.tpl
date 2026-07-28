{{- define "dependency-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dependency-controller.fullname" -}}
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

{{- define "dependency-controller.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "dependency-controller.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.controller.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "dependency-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dependency-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end }}

{{- define "dependency-controller.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.create }}
{{- default (include "dependency-controller.fullname" .) .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Webhook helpers
*/}}

{{- define "dependency-controller.webhook.fullname" -}}
{{- printf "%s-webhook" (include "dependency-controller.fullname" .) }}
{{- end }}

{{- define "dependency-controller.webhook.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dependency-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: webhook
{{- end }}

{{- define "dependency-controller.webhook.serviceAccountName" -}}
{{- if .Values.webhook.serviceAccount.create }}
{{- default (include "dependency-controller.webhook.fullname" .) .Values.webhook.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.webhook.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "dependency-controller.webhook.tlsSecretName" -}}
{{- if .Values.webhook.tls.existingSecret }}
{{- .Values.webhook.tls.existingSecret }}
{{- else }}
{{- printf "%s-tls" (include "dependency-controller.webhook.fullname" .) }}
{{- end }}
{{- end }}

{{- define "dependency-controller.webhook.serviceFQDN" -}}
{{ include "dependency-controller.webhook.fullname" . }}.{{ .Release.Namespace }}.svc
{{- end }}
