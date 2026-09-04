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
The serving backends the release runs, as a JSON list: `backends` when set,
else `[backend]`. The first is the default backend.
*/}}
{{- define "model-manager.backends" -}}
{{- if .Values.backends }}{{ .Values.backends | toJson }}{{ else }}{{ list .Values.backend | toJson }}{{ end }}
{{- end }}

{{/*
Truthy when the named driver is among the release's backends.
Usage: include "model-manager.hasBackend" (dict "root" . "name" "kserve")
*/}}
{{- define "model-manager.hasBackend" -}}
{{- if has .name (include "model-manager.backends" .root | fromJsonArray) }}true{{ end }}
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
The platform identity contract (global.identity), an empty dict when absent.
*/}}
{{- define "model-manager.globalIdentity" -}}
{{- dig "identity" (dict) (.Values.global | default dict) | toJson }}
{{- end }}

{{/*
Existing Secret with the provider credentials: oauth.existingSecret, else the
platform's global.identity.existingSecret, else the chart-rendered one.
*/}}
{{- define "model-manager.oauthSecretName" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.existingSecret | default (dig "existingSecret" "" $g) | default (printf "%s-oauth" (include "model-manager.fullname" .)) }}
{{- end }}

{{/*
Whether the chart renders its own OAuth Secret (no existing one named).
*/}}
{{- define "model-manager.oauthRendersSecret" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- if and .Values.oauth.enabled (not .Values.oauth.existingSecret) (not (dig "existingSecret" "" $g)) }}true{{ end }}
{{- end }}

{{/*
OAuth base URL: oauth.baseURL, else https://<fullname>.<global.domain>.
*/}}
{{- define "model-manager.oauthBaseURL" -}}
{{- $domain := dig "domain" "" (.Values.global | default dict) -}}
{{- $derived := "" -}}
{{- if $domain }}{{ $derived = printf "https://%s.%s" (include "model-manager.fullname" .) $domain }}{{ end -}}
{{- required "oauth.baseURL is required when oauth.enabled (or set global.domain)" (.Values.oauth.baseURL | default $derived) }}
{{- end }}

{{/*
Dex issuer / client id with the global.identity fallbacks.
*/}}
{{- define "model-manager.oauthDexIssuerURL" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.issuerURL (or global.identity.issuerUrl) is required for the dex provider" (.Values.oauth.dex.issuerURL | default (dig "issuerUrl" "" $g)) }}
{{- end }}

{{- define "model-manager.oauthDexClientID" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.clientID (or global.identity.clientId) is required for the dex provider" (.Values.oauth.dex.clientID | default (dig "clientId" "" $g)) }}
{{- end }}

{{/*
CA Secret of a private-certificate Dex: oauth.dex.caSecret, else
global.identity.ca. Name empty means system trust.
*/}}
{{- define "model-manager.oauthDexCASecretName" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.dex.caSecret.name | default (dig "ca" "secretName" "" $g) }}
{{- end }}

{{- define "model-manager.oauthDexCASecretKey" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- if .Values.oauth.dex.caSecret.name }}{{ .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ else }}{{ dig "ca" "key" "" $g | default .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ end }}
{{- end }}

{{/*
Trusted audiences, comma-separated: the OAuth client ids whose IdP id_tokens
this server accepts as bearer tokens. The union, in this order and without
duplicates, of
  - oauth.trustedAudiences, else the platform client (global.identity.clientId)
    — the client MCP clients and the muster CLI log in with;
  - muster.mcpServer.auth.requiredAudiences — every token muster forwards to
    this server carries them by construction (muster requests them at login)
    and they are the audiences the kube-apiserver trusts, so a portal
    session's token, which carries them but not the platform client, is
    accepted too. The pair is what the management clusters' mcp-kubernetes
    trusts as well.
A Google install has no cross-client audiences (requiredAudiences empty) and
trusts the client id alone.
*/}}
{{- define "model-manager.oauthTrustedAudiences" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- $base := .Values.oauth.trustedAudiences | default (list (dig "clientId" "" $g)) -}}
{{- $auds := list -}}
{{- range concat $base (.Values.muster.mcpServer.auth.requiredAudiences | default list) -}}
{{- if and . (not (has . $auds)) }}{{ $auds = append $auds . }}{{ end -}}
{{- end -}}
{{- join "," $auds -}}
{{- end }}
