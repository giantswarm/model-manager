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
token-server lifecycle. `cmd/tool_policy.go` translates validated user info to
plain toolkit identity fields. GitHub grant orchestration remains product code;
it is not an OAuth configuration or generic MCP transport responsibility.

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
delete a shared download or serving object. Stop the underlying work through the
Kubernetes resource API with the appropriate RBAC permissions, or use
`unload_model` to stop serving. These operations use the acting caller's
authority. The Kubernetes Job controller owns execution of submitted downloads.
Downloads on different node caches have distinct Job identities; a matching
existing Job can be joined after the caller's Kubernetes access is checked.

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

## Operator notice and recovery

Before rollout, notify API users of the default tool limits, 413 response,
stateless transport, Origin checks and gateway/scraper selectors. Confirm the
actual REST clients and gateway peers for the installation. Send the notice
through the installation's approved channel before changing network access.

Notice draft:

```text
The model-manager update adds private user/client/actor-bound task handles, default tool limits of 30 seconds and 128 KiB, and REST 413 above 1 MiB.
Cancelling a KServe task stops tracking; shared downloads continue. Resource operations retain caller RBAC, and chart ingress is limited to configured gateways and scrapers.
Refresh rejected credentials through sign-in. Confirm client identity claims and report access failures before rollout; authentication remains enforced during recovery.
```

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
