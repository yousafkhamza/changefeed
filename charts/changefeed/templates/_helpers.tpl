{{- define "changefeed.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "changefeed.fullname" -}}
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

{{- define "changefeed.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "changefeed.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "changefeed.selectorLabels" -}}
app.kubernetes.io/name: {{ include "changefeed.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "changefeed.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "changefeed.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* RBAC rules for the resources the Kubernetes collector watches. Read-only. */}}
{{- define "changefeed.rules" -}}
{{- $watch := .Values.config.collectors.kubernetes.watch }}
{{- $apps := list }}
{{- range $r := list "deployments" "statefulsets" "daemonsets" }}
{{- if has $r $watch }}{{- $apps = append $apps $r }}{{- end }}
{{- end }}
{{- if $apps }}
- apiGroups: ["apps"]
  resources: {{ toJson $apps }}
  verbs: ["get", "list", "watch"]
{{- end }}
{{- if has "configmaps" $watch }}
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch"]
{{- end }}
{{- if has "helm" $watch }}
# Helm stores releases as Secrets. changefeed requests only their metadata
# (labels name/version/status) and never reads Secret data, but Kubernetes
# RBAC cannot grant metadata-only access, so list/watch on secrets is needed.
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["list", "watch"]
{{- end }}
{{- end }}
