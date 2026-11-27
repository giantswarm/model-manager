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
Chart label value. A label value must begin and end alphanumeric: the
63-character cut of a branch build's version (0.x.y-dev.<branch>.<timestamp>.<sha>)
can land on any run of ".", "_" (from "+") and "-", so the whole run is trimmed.
*/}}
{{- define "model-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimAll "-._" }}
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
Serving namespace of the kserve backend (LLMInferenceServices, download Jobs,
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
Existing Secret with the provider credentials of the deprecated keys:
oauth.existingSecret, else the platform's global.identity.existingSecret,
else the chart-rendered one.
*/}}
{{- define "model-manager.oauthSecretName" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.existingSecret | default (dig "existingSecret" "" $g) | default (printf "%s-oauth" (include "model-manager.fullname" .)) }}
{{- end }}

{{/*
Whether the chart renders its own OAuth Secret: no
authorizationServer.upstream.client.secretKeyRef, no existing Secret named.
*/}}
{{- define "model-manager.oauthRendersSecret" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- $o := .Values.oauth -}}
{{- if and $o.enabled $o.authorizationServer.enabled (not $o.authorizationServer.upstream.client.secretKeyRef.name) (not $o.existingSecret) (not (dig "existingSecret" "" $g)) }}true{{ end }}
{{- end }}

{{/*
The provider people sign in with: authorizationServer.upstream.type, else the
deprecated oauth.provider, else global.identity.type, else dex.
*/}}
{{- define "model-manager.oauthProvider" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.authorizationServer.upstream.type | default .Values.oauth.provider | default (dig "type" "" $g) | default "dex" }}
{{- end }}

{{/*
The audiences, as JSON, that muster requests at login and the forwarded ID
token then carries for the Kubernetes API server: muster.mcpServer.auth.requiredAudiences,
else global.identity.kubernetes.audiences.
*/}}
{{- define "model-manager.requiredAudiences" -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- .Values.muster.mcpServer.auth.requiredAudiences | default (dig "kubernetes" "audiences" list $g) | toJson }}
{{- end }}

{{/*
The OAuth settings the Deployment renders, as JSON. Each comes from the
shared layout (oauth.resourceServer, oauth.authorizationServer), else from
the deprecated key it replaces, else from global.identity:
  issuer          authorizationServer.issuer, baseURL, https://<fullname>.<global.domain>
  provider        authorizationServer.upstream.type, provider, global.identity.type, dex
  upstreamIssuer  authorizationServer.upstream.issuer, dex.issuerURL, global.identity.issuerUrl
  clientID        authorizationServer.upstream.client.id, dex.clientID or google.clientID,
                  global.identity.clientId (dex)
  clientSecret    authorizationServer.upstream.client.secretKeyRef, existingSecret,
                  global.identity.existingSecret, the chart-rendered Secret
  upstreamCA      authorizationServer.upstream.certificateAuthority.secretKeyRef,
                  dex.caSecret, global.identity.ca
  trustedIssuers  resourceServer.trustedIssuers, else one entry for the
                  provider people sign in with (openIDProvider), accepting its
                  ID tokens for the union, in this order and without
                  duplicates, of the deprecated trustedAudiences (else
                  clientID, else global.identity.clientId) and
                  model-manager.requiredAudiences: every token muster
                  forwards carries those by construction and the
                  kube-apiserver trusts them, so a portal session's ID token
                  is accepted too
A CA is a mount: {volume, secret or configMap, key, dir}; caFile is dir/key.
*/}}
{{- define "model-manager.oauth" -}}
{{- $o := .Values.oauth -}}
{{- $as := $o.authorizationServer -}}
{{- $up := $as.upstream -}}
{{- $rs := $o.resourceServer -}}
{{- $g := include "model-manager.globalIdentity" . | fromJson -}}
{{- $domain := dig "domain" "" (.Values.global | default dict) -}}
{{- $derived := "" -}}
{{- if $domain }}{{ $derived = printf "https://%s.%s" (include "model-manager.fullname" .) $domain }}{{ end -}}
{{- $r := dict "authorizationServer" $as.enabled "mounts" list -}}
{{- $_ := set $r "issuer" (required "oauth.authorizationServer.issuer is required when oauth.enabled (or set global.domain)" ($as.issuer | default $o.baseURL | default $derived)) -}}
{{- $provider := include "model-manager.oauthProvider" . -}}
{{- $_ = set $r "provider" $provider -}}
{{- $upstreamIssuer := "" -}}
{{- $clientID := "" -}}
{{- if eq $provider "dex" -}}
{{- $upstreamIssuer = $up.issuer | default $o.dex.issuerURL | default (dig "issuerUrl" "" $g) -}}
{{- $clientID = $up.client.id | default $o.dex.clientID | default (dig "clientId" "" $g) -}}
{{- else -}}
{{- $upstreamIssuer = "https://accounts.google.com" -}}
{{- $clientID = $up.client.id | default $o.google.clientID -}}
{{- end -}}
{{- $_ = set $r "upstreamIssuer" $upstreamIssuer -}}
{{- $_ = set $r "clientID" $clientID -}}
{{- if $up.client.secretKeyRef.name -}}
{{- $_ = set $r "clientSecret" (dict "name" $up.client.secretKeyRef.name "key" (required "oauth.authorizationServer.upstream.client.secretKeyRef.key is required with its name" $up.client.secretKeyRef.key)) -}}
{{- else -}}
{{- $_ = set $r "clientSecret" (dict "name" (include "model-manager.oauthSecretName" .) "key" (printf "%s-client-secret" $provider)) -}}
{{- if and (eq $provider "google") (not $clientID) -}}
{{- $_ = set $r "clientIDSecret" (dict "name" (include "model-manager.oauthSecretName" .) "key" "google-client-id") -}}
{{- end -}}
{{- end -}}
{{- if eq $provider "dex" -}}
{{- $ca := dict -}}
{{- if $up.certificateAuthority.secretKeyRef.name -}}
{{- $ca = dict "secret" $up.certificateAuthority.secretKeyRef.name "key" ($up.certificateAuthority.secretKeyRef.key | default "ca.crt") -}}
{{- else if $o.dex.caSecret.name -}}
{{- $ca = dict "secret" $o.dex.caSecret.name "key" ($o.dex.caSecret.key | default "ca.crt") -}}
{{- else if dig "ca" "secretName" "" $g -}}
{{- $ca = dict "secret" (dig "ca" "secretName" "" $g) "key" (dig "ca" "key" "" $g | default $o.dex.caSecret.key | default "ca.crt") -}}
{{- end -}}
{{- if $ca.secret -}}
{{- $_ = set $ca "volume" "idp-ca" -}}
{{- $_ = set $ca "dir" "/etc/model-manager/idp-ca" -}}
{{- $_ = set $r "upstreamCA" $ca -}}
{{- $_ = set $r "mounts" (append $r.mounts $ca) -}}
{{- end -}}
{{- end -}}
{{- $issuers := list -}}
{{- range $i, $entry := $rs.trustedIssuers -}}
{{- $out := omit $entry "certificateAuthority" -}}
{{- with $entry.certificateAuthority -}}
{{- $ca := dict "volume" (printf "trusted-issuer-ca-%d" $i) "dir" (printf "/etc/model-manager/trusted-issuer-ca/%d" $i) -}}
{{- if .secretKeyRef -}}
{{- $_ := set $ca "secret" (required (printf "oauth.resourceServer.trustedIssuers[%d].certificateAuthority.secretKeyRef.name is required" $i) .secretKeyRef.name) -}}
{{- $_ = set $ca "key" (.secretKeyRef.key | default "ca.crt") -}}
{{- else -}}
{{- $_ := set $ca "configMap" (required (printf "oauth.resourceServer.trustedIssuers[%d].certificateAuthority needs secretKeyRef or configMapKeyRef" $i) (dig "configMapKeyRef" "name" "" .)) -}}
{{- $_ = set $ca "key" (dig "configMapKeyRef" "key" "" . | default "ca.crt") -}}
{{- end -}}
{{- $_ := set $out "caFile" (printf "%s/%s" $ca.dir $ca.key) -}}
{{- $_ = set $r "mounts" (append $r.mounts $ca) -}}
{{- end -}}
{{- $issuers = append $issuers $out -}}
{{- end -}}
{{- if not $issuers -}}
{{- $audiences := list -}}
{{- range concat ($o.trustedAudiences | default (list ($clientID | default (dig "clientId" "" $g)))) (include "model-manager.requiredAudiences" . | fromJsonArray) -}}
{{- if and . (not (has . $audiences)) }}{{ $audiences = append $audiences . }}{{ end -}}
{{- end -}}
{{- if and $audiences $upstreamIssuer -}}
{{- $entry := dict "issuer" $upstreamIssuer "audiences" $audiences "openIDProvider" true -}}
{{- if eq $provider "dex" -}}
{{- /* Dex serves its JWKS at <issuer>/keys and signs ID tokens without a typ header. mcp-oauth does not imply the issuer host for private JWKS addresses, so it is listed. */ -}}
{{- $_ := set $entry "jwksURI" (printf "%s/keys" (trimSuffix "/" $upstreamIssuer)) -}}
{{- $_ = set $entry "acceptedTypHeaders" (list "" "JWT") -}}
{{- $_ = set $entry "allowPrivateIPJWKSHosts" (concat (list (urlParse $upstreamIssuer).hostname) $o.sso.allowPrivateIPJWKSHosts | uniq) -}}
{{- with $r.upstreamCA }}{{ $_ = set $entry "caFile" (printf "%s/%s" .dir .key) }}{{ end -}}
{{- else -}}
{{- $_ := set $entry "jwksURI" "https://www.googleapis.com/oauth2/v3/certs" -}}
{{- $_ = set $entry "acceptedTypHeaders" (list "JWT") -}}
{{- end -}}
{{- $issuers = list $entry -}}
{{- end -}}
{{- end -}}
{{- if not $as.enabled -}}
{{- if not $issuers -}}
{{- fail "oauth.authorizationServer.enabled false accepts only trusted issuer tokens: set oauth.resourceServer.trustedIssuers, or the upstream issuer and client ID it is derived from" -}}
{{- end -}}
{{- if not $rs.authorizationServers -}}
{{- fail "oauth.authorizationServer.enabled false needs oauth.resourceServer.authorizationServers: MCP clients discover the authorization server from the protected resource metadata (RFC 9728), and this server then runs none" -}}
{{- end -}}
{{- end -}}
{{- $_ = set $r "trustedIssuers" $issuers -}}
{{- toJson $r -}}
{{- end }}

{{/*
The OTLP collector the traces go to, as "<namespace>:<port>" for an
in-cluster Service host (<svc>.<namespace>[.svc[.cluster.local]]), else
":<port>". Without a port in the endpoint: 443 for https, else the
protocol's (4317 gRPC, 4318 HTTP). Empty without an endpoint.
*/}}
{{- define "model-manager.otlpTarget" -}}
{{- $otel := .Values.observability.otel -}}
{{- with $otel.endpoint -}}
{{- $endpoint := . -}}
{{- if not (contains "://" $endpoint) -}}
{{- $endpoint = printf "http://%s" $endpoint -}}
{{- end -}}
{{- $url := urlParse $endpoint -}}
{{- $hostPort := splitList ":" $url.host -}}
{{- $port := "4317" -}}
{{- if eq (len $hostPort) 2 -}}
{{- $port = index $hostPort 1 -}}
{{- else if eq $url.scheme "https" -}}
{{- $port = "443" -}}
{{- else if eq $otel.protocol "http/protobuf" -}}
{{- $port = "4318" -}}
{{- end -}}
{{- $labels := splitList "." (first $hostPort) -}}
{{- $namespace := "" -}}
{{- if or (eq (len $labels) 2) (and (ge (len $labels) 3) (eq (index $labels 2) "svc")) -}}
{{- $namespace = index $labels 1 -}}
{{- end -}}
{{- printf "%s:%s" $namespace $port -}}
{{- end -}}
{{- end }}

{{/*
"true" when github.tokenExchange is on (tokenEndpoint set), after refusing an
incomplete or contradictory configuration: github.enabled needs the exchange,
and the exchange needs github.enabled,
Dex OAuth with downstream OAuth and muster's forwardToken (the person's ID
token is the subject token), an audience, and a client secret (existingSecret or the inline clientSecret).
*/}}
{{- define "model-manager.githubTokenExchange" -}}
{{- $x := .Values.github.tokenExchange -}}
{{- if $x.tokenEndpoint -}}
{{- if not .Values.github.enabled -}}
{{- fail "github.tokenExchange.tokenEndpoint is set and github.enabled is false: set github.enabled to offer commit mode with the token exchange" -}}
{{- end -}}
{{- if not (and .Values.oauth.enabled (eq (include "model-manager.oauthProvider" .) "dex") .Values.oauth.downstream.enabled) -}}
{{- fail "github.tokenExchange exchanges the person's Dex ID token: set oauth.enabled with the dex provider (oauth.authorizationServer.upstream.type) and oauth.downstream.enabled" -}}
{{- end -}}
{{- if not $x.audience -}}
{{- fail "github.tokenExchange.audience is required with github.tokenExchange.tokenEndpoint" -}}
{{- end -}}
{{- if not .Values.muster.mcpServer.auth.forwardToken -}}
{{- fail "github.tokenExchange needs muster.mcpServer.auth.forwardToken: the exchange's subject is the forwarded ID token, and muster refuses auth.providerApps without it" -}}
{{- end -}}
{{- if not (or $x.existingSecret $x.clientSecret) -}}
{{- fail "github.tokenExchange needs existingSecret (preferred) or clientSecret" -}}
{{- end -}}
true
{{- else if or $x.audience $x.clientID $x.clientSecret $x.existingSecret $x.caSecretName -}}
{{- fail "github.tokenExchange.tokenEndpoint is required when any other github.tokenExchange value is set" -}}
{{- else if .Values.github.enabled -}}
{{- fail "github.enabled needs github.tokenExchange.tokenEndpoint: commit mode fetches the person's GitHub App user access token per commit from muster's broker (github.tokenExchange)" -}}
{{- end -}}
{{- end }}

{{/*
Secret with the token exchange client's client-secret (and client-id):
github.tokenExchange.existingSecret, else the chart-rendered one.
*/}}
{{- define "model-manager.githubTokenExchangeSecretName" -}}
{{- .Values.github.tokenExchange.existingSecret | default (printf "%s-github-token-exchange" (include "model-manager.fullname" .)) }}
{{- end }}
