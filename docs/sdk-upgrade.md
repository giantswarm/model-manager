# MCP policy and access upgrade

The server uses native mcp-go stateless streamable HTTP. It does not require a
session identifier between requests. HTTP clients negotiating `2024-11-05`,
`2025-03-26`, `2025-06-18` and `2025-11-25` remain covered by transport tests.
Native 2026-07-28 clients use `server/discover` and per-request metadata;
discovery and an actual tool call are covered with the mcp-go client.
Browser callers must send a same-origin Origin. Non-browser clients can omit it.

Tool calls default to a 30-second deadline and a 128 KiB complete JSON result
limit. Use `MODEL_MANAGER_TOOL_TIMEOUT` and `MODEL_MANAGER_MAX_RESPONSE_BYTES`
or the matching flags to configure these limits. Nonpositive values disable
the respective limit. REST requests larger than 1 MiB receive 413 problem
details; the existing `error.code` and `error.message` fields remain available.
Backend failures expose stable codes rather than raw upstream response bodies.
Refresh a rejected caller credential through the existing sign-in flow.

## Access and ownership

OAuth setup uses mcp-oauth's `oauthconfig`; Kubernetes caller clients use the
separate `mcp-toolkit/k8s` module. Existing provider flags, environment settings,
custom trust roots and the tracked token-audience behavior are retained.
Persistent Valkey token storage requires encryption. A failed caller-client
construction sends no ServiceAccount request. A job retains its caller token
even after expiry and surfaces the API's rejection.

The product adapter in `internal/server/oauth.go` retains resolved CLI settings,
resource-path selection, attachment of caller credentials for Kubernetes and
application cleanup logging. `oauthconfig` owns provider/store construction and
token-server lifecycle. Tool auditing logs mcp-oauth's validated
`providers.UserInfo` under `user`. Fetching the GitHub user access token remains
product code; it is not an OAuth configuration or generic MCP transport
responsibility.

### OAuth configuration

Deprecated flags, environment variables and chart values remain supported
throughout model-manager 1.x. The no-op values `oauth.dex.allowPrivateURLs`
and `oauth.sso.allowPrivateIPs` remain accepted by the chart schema. Removing
these public configuration options requires a major release; upgrading within
1.x does not require deleting them.

The chart renders mcp-oauth's `OAUTH_*` variables from
`oauth.resourceServer` and `oauth.authorizationServer`. Prefer these values
for new installations. New chart keys take precedence over deprecated keys,
then `global.identity` supplies defaults. For the binary, an explicit
`OAUTH_*` variable takes precedence over a deprecated option.

| Deprecated configuration | Replacement |
| --- | --- |
| `oauth.baseURL` | `oauth.authorizationServer.issuer` |
| `oauth.provider` | `oauth.authorizationServer.upstream.type` |
| `oauth.dex.issuerURL` | `oauth.authorizationServer.upstream.issuer` |
| `oauth.dex.clientID`, `oauth.google.clientID` | `oauth.authorizationServer.upstream.client.id` |
| `oauth.existingSecret`, inline provider client secrets | `oauth.authorizationServer.upstream.client.secretKeyRef` |
| `oauth.dex.caSecret` | `oauth.authorizationServer.upstream.certificateAuthority.secretKeyRef` |
| `oauth.trustedAudiences`, `oauth.sso.allowPrivateIPJWKSHosts` | `audiences`, `allowPrivateIPJWKSHosts` on `oauth.resourceServer.trustedIssuers` entries |
| `--oauth-base-url`, `MODEL_MANAGER_OAUTH_BASE_URL` | `OAUTH_ISSUER` |
| `--oauth-provider`, `MODEL_MANAGER_OAUTH_PROVIDER` | `OAUTH_PROVIDER` |
| `--dex-*`, `DEX_*` | Corresponding `OAUTH_DEX_*` variables; mounted credentials use `_FILE` |
| `--google-*`, `GOOGLE_*` | Corresponding `OAUTH_GOOGLE_*` variables; mounted credentials use `_FILE` |
| `--oauth-trusted-audiences` | `OAUTH_TRUSTED_AUDIENCES`, or issuer-specific audiences in `OAUTH_TRUSTED_ISSUERS` |
| `--sso-allow-private-ip-jwks-hosts`, `SSO_ALLOW_PRIVATE_IP_JWKS_HOSTS` | `OAUTH_ALLOW_PRIVATE_IP_JWKS_HOSTS` |
| `--allow-public-client-registration`, `MODEL_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION` | `OAUTH_ALLOW_PUBLIC_CLIENT_REGISTRATION` |
| `OAUTH_REGISTRATION_ACCESS_TOKEN`, `OAUTH_REGISTRATION_ACCESS_TOKEN_FILE` | `OAUTH_INITIAL_ACCESS_TOKEN`, `OAUTH_INITIAL_ACCESS_TOKEN_FILE` |
| `oauth.dex.allowPrivateURLs`, `oauth.sso.allowPrivateIPs`, and their no-op flags | No replacement needed |

Keep trusted issuer lists and CAs in release values or administrator-owned
GitOps ConfigMaps. Preserve the issuer, audiences, client and CA when moving
keys. A rejected credential needs fresh sign-in. If a configuration migration
fails, restore the previous values through GitOps and retry with the same
caller RBAC; keep OAuth and downstream authentication enabled.

### Resource and task ownership

| State | Key and authority | Upgrade action |
| --- | --- | --- |
| ModelConfig ownership | model-manager instance label and resource identity | Keep instance labels and resource names |
| Serving resources and cache indexes | backend, namespace, model and Kubernetes resource identity | Keep resource names and Kubernetes RBAC |
| Backend documents | ConfigMap namespace/name and backend name | Keep ConfigMaps and explicit targets |
| GitOps ownership | Flux object and repository path | Keep Flux provenance and provider grants |
| In-memory tasks | validated issuer/subject/client/current actor; grant and complete parameters scope deduplication | New private handles after restart; join backing work through caller RBAC |
| Job requestedBy | display attribution only, email or subject | No authorization or ownership mapping |

Job progress and cancellation require the same validated issuer, subject, OAuth
client and current delegated actor. Unknown and foreign task IDs return 404;
lists omit inaccessible tasks. `requestedBy` is display attribution only. A
changed email does not change ownership. A different connector, client or actor
does not acquire access by presenting the same email. Missing or ambiguous
client identity is refused with 403: forwarded ID tokens need a signed `azp` or
a single audience; supported access tokens carry their validated client binding.

Kubernetes task reads and cancellation recheck resource access using the current
caller's credential and the original resource namespace/name/UID. Resource
replacement is refused, and lists omit that task. A garbage-collected object
still requires the caller's read permission: the API server authorizes the read
before reporting the object as missing. A refreshed credential can read an
owned task, but does not reuse a worker holding an older credential.

Cancelling a KServe task stops its observer and follow-up wiring; it does not
delete a shared download or serving object. Stop a download with
`stop_download` (below), or use `unload_model` to stop serving. These operations use the acting caller's
authority. The Kubernetes Job controller owns execution of submitted downloads.
Downloads on different node caches have distinct Job identities; a matching
existing Job can be joined after the caller's Kubernetes access is checked.

### Stopping a shared download

A task is bound to the caller's authorization context, as the MCP tasks
specification requires, so cancelling it never ends another caller's task. The
download Job is shared work. `stop_download` (`POST
/api/v1/models/stop-download`) deletes it with the caller's own Kubernetes
credentials:

- The caller needs `delete` on `batch/jobs` in the serving namespace. The
  cluster refuses everyone else with 403.
- The delete carries the Job's UID as a precondition, so a Job recreated under
  the same name is never stopped by mistake. `uid` narrows it to the object a
  task reported.
- `dryRun` asks the cluster to admit the delete without performing it.
- Every task observing the Job fails with `download stopped`. Partial files
  stay in the cache for the next `pull_model`.
- Without downstream OAuth the delete would run with the ServiceAccount's
  permissions, so the call answers `unsupported` and the Job is deleted through
  the Kubernetes API instead.

This is how KServe, KAITO, Fluid and OME treat a shared download: one object,
deleting it stops the work for everyone, and RBAC decides who may delete it.

KServe's `LocalModelCache` was evaluated as the download object
(giantswarm/model-manager#446) and is not used yet:

- Deleting a cache also stops the download for every caller and removes the
  files. Its in-use check covers serving workloads, not other callers.
- The node-group mode writes to a hostPath on every node of the group. That
  does not fit a download pinned to one node's claim, or a model split across
  nodes. The shared-claim mode (`pvcRef`) needs a ReadWriteMany claim and is
  not in a KServe release (v0.21.0).
- Its status has no byte progress, and a failed download is not retried until
  someone deletes it.
- The KServe app does not ship its controller, node agent, CRDs or webhook.
- `LocalModelCache` is cluster-scoped, so callers would need cluster-wide
  delete permission.

Revisit it once a KServe release ships `pvcRef`, the KServe app ships the
local model components, and the platform offers a ReadWriteMany cache class.
`stop_download` would then delete the namespaced cache as the caller.

Task state is in memory, so restart requires a new private handle. A fresh pull
can join an existing matching download through the caller's Kubernetes access.
Application-owned recovered tasks are not assigned to a user or exposed as user
tasks. No credentials are stored in resource ownership annotations. There is no
email-based account migration. Before any future persisted owner-key change, require verified
issuer/subject mappings, reject collisions and unresolved mappings, and verify
repeat execution before cutover. An email match alone is insufficient.

## Network access

The chart enables NetworkPolicy by default. API ingress allows same-namespace
Muster and agentgateway pods. Metrics ingress allows kube-system Alloy pods
labelled `app.kubernetes.io/instance=alloy-metrics`. Configure
`networkPolicy.gatewayPeers` and `networkPolicy.metricsPeers` for other data
planes and collectors. A namespace selector and pod selector in one peer must
both match. Explicit legacy `ingressNamespaces` and `metricsNamespaces` remain
additional namespace-wide permissions during overlap.

NetworkPolicy restricts TCP peers and ports; it cannot restrict HTTP paths.
Gateway routing and application authentication still enforce HTTP access.
When using an HTTPRoute, configure the actual gateway's data-plane selector.
ServiceMonitors require `serviceMonitor.labels.observability.giantswarm.io/tenant`,
defaulting to `giantswarm`. Set the intended tenant before enabling a monitor.

Egress allows DNS, port 443 and 6443 (with `allowKubeAPI`), the OTLP collector
and the KServe serving namespace. A backend on another port, such as an
in-cluster or host Ollama, Lemonade or LM Studio endpoint, needs its address in
`networkPolicy.egressCIDRs`, or NetworkPolicy disabled for that development
installation with `--set networkPolicy.enabled=false`. Keep OAuth enabled
where the server is reachable by other users.

## Recovery

If a supported client loses access, check its negotiated version, Origin,
resource metadata, caller credential and selected network peer. Correct the
explicit peer selector or limit setting through GitOps and retry the same
authenticated operation. Retain OAuth, downstream caller authentication and
Kubernetes RBAC throughout recovery. An expired token needs fresh authentication,
not ServiceAccount substitution. Keep the previous chart revision available
for a GitOps rollback; do not restore the caller-client fallback.

Shutdown uses one configurable 25-second process budget with five seconds
reserved for telemetry. Readiness is revoked before draining, OAuth remains
alive during HTTP drain, and jobs stop accepting work and receive cancellation.
Confirm the pod termination grace period exceeds the configured process budget.
