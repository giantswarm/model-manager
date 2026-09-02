{{/*
Expand the name of the chart.
*/}}
{{- define "model-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "model-manager.fullname" -}}
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
Chart label value.
*/}}
{{- define "model-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "model-manager.labels" -}}
helm.sh/chart: {{ include "model-manager.chart" . }}
{{ include "model-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "model-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "model-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "model-manager.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "model-manager.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Serving namespace of the kserve backend (InferenceServices, download Jobs,
cache); defaults to the release namespace.
*/}}
{{- define "model-manager.kserveNamespace" -}}
{{- default .Release.Namespace .Values.kserve.namespace }}
{{- end }}

{{/*
Namespace of the model-serving discovery ConfigMap; defaults to the release
namespace (where the umbrella chart publishes it).
*/}}
{{- define "model-manager.kserveDiscoveryNamespace" -}}
{{- default .Release.Namespace .Values.kserve.discovery.namespace }}
{{- end }}

{{/*
Namespace of the serving-preset ConfigMaps; defaults to the discovery namespace.
*/}}
{{- define "model-manager.kservePresetNamespace" -}}
{{- default (include "model-manager.kserveDiscoveryNamespace" .) .Values.kserve.presets.namespace }}
{{- end }}

{{/*
Labels selecting this release's cache-agent pods (kserve.inventory.mode:
daemonset) — distinct from the API pods so the Service never routes to them.
*/}}
{{- define "model-manager.cacheAgentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "model-manager.name" . }}-cache-agent
app.kubernetes.io/instance: {{ .Release.Name }}
model-manager.giantswarm.io/component: cache-agent
{{- end }}

{{/*
The same selector as a label-selector string (passed to the Deployment).
*/}}
{{- define "model-manager.cacheAgentSelector" -}}
model-manager.giantswarm.io/component=cache-agent,app.kubernetes.io/instance={{ .Release.Name }}
{{- end }}

{{/*
Name of the Secret holding the Dex client secret when OAuth is enabled.
*/}}
{{- define "model-manager.oauthSecretName" -}}
{{- default (printf "%s-oauth" (include "model-manager.fullname" .)) .Values.oauth.existingSecret }}
{{- end }}
