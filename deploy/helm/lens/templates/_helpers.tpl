{{/*
Expand the name of the chart.
*/}}
{{- define "lens.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name (release-scoped, ≤63 chars).
*/}}
{{- define "lens.fullname" -}}
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
Chart name and version, as used by the helm.sh/chart label.
*/}}
{{- define "lens.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "lens.labels" -}}
helm.sh/chart: {{ include "lens.chart" . }}
{{ include "lens.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: talyvor
{{- end }}

{{/*
Selector labels (stable across upgrades — do not add version here).
*/}}
{{- define "lens.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lens.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Component selector labels — pass the component name as the second arg.
Usage: include "lens.componentSelectorLabels" (list . "gateway")
*/}}
{{- define "lens.componentSelectorLabels" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
{{ include "lens.selectorLabels" $root }}
app.kubernetes.io/component: {{ $component }}
{{- end }}

{{/*
The image reference, defaulting the tag to the chart appVersion.
*/}}
{{- define "lens.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
The name of the Secret the gateway reads env from: the operator-supplied
existingSecret if set, otherwise the chart's conventional "<fullname>-env".
*/}}
{{- define "lens.secretName" -}}
{{- if .Values.secret.existingSecret -}}
{{- .Values.secret.existingSecret -}}
{{- else -}}
{{- printf "%s-env" (include "lens.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
ServiceAccount name to use.
*/}}
{{- define "lens.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "lens.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}
