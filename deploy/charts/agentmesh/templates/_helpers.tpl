{{/*
Expand the name of the chart.
*/}}
{{- define "agentmesh.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
Truncated at 63 characters because Kubernetes limits label values to 63 chars.
If fullnameOverride is set, use it directly.
Otherwise: <release-name>-<chart-name>, or just <release-name> when the
release name already contains the chart name.
*/}}
{{- define "agentmesh.fullname" -}}
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
Create chart label value (name-version).
*/}}
{{- define "agentmesh.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "agentmesh.labels" -}}
helm.sh/chart: {{ include "agentmesh.chart" . }}
{{ include "agentmesh.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels — used by both Deployment.spec.selector and Service.spec.selector.
Must be stable across upgrades (do not include version or chart).
*/}}
{{- define "agentmesh.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agentmesh.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the ServiceAccount to use.
*/}}
{{- define "agentmesh.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "agentmesh.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the ConfigMap holding agentmesh.yaml.
*/}}
{{- define "agentmesh.configmapName" -}}
{{- printf "%s-config" (include "agentmesh.fullname" .) }}
{{- end }}
