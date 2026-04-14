{{- define "dependency-webhook.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dependency-webhook.fullname" -}}
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

{{- define "dependency-webhook.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "dependency-webhook.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "dependency-webhook.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dependency-webhook.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "dependency-webhook.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "dependency-webhook.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
TLS secret name: use existingSecret if set, otherwise the cert-manager-generated name.
*/}}
{{- define "dependency-webhook.tlsSecretName" -}}
{{- if .Values.tls.existingSecret }}
{{- .Values.tls.existingSecret }}
{{- else }}
{{- printf "%s-tls" (include "dependency-webhook.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Webhook service FQDN used in cert-manager Certificate dnsNames and as the
default webhook URL for the controller.
*/}}
{{- define "dependency-webhook.serviceFQDN" -}}
{{ include "dependency-webhook.fullname" . }}.{{ .Release.Namespace }}.svc
{{- end }}
