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
| In-memory jobs | random job ID; deduplication by operation/backend/model | Drain or cancel at shutdown; job state is not persisted |
| Job requestedBy | display attribution only, email or subject | No authorization or ownership mapping |

Job progress and cancellation are process-wide: an authenticated caller can
inspect and cancel another caller's job. Review that shared-access contract
before enabling multi-user access; `requestedBy` is not an authorization check.

These keys do not change to user ownership during this upgrade. There is no
email-based account migration to run. Tool audit identity uses validated issuer,
subject and actor. Kubernetes authorization continues to use the caller's
credential. Before any future persisted owner-key change, require verified
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

For a local cluster using host backends, either set the host's explicit
`networkPolicy.egressCIDRs` or disable NetworkPolicy for that development
installation with `--set networkPolicy.enabled=false`. Keep OAuth enabled
where the server is reachable by other users.

## Operator notice and recovery

Before rollout, notify API users of the default tool limits, 413 response,
stateless transport, Origin checks and gateway/scraper selectors. Confirm the
actual REST clients and gateway peers for the installation. Send the notice
through the installation's approved channel before changing network access.

Notice draft:

```text
The model-manager update adds default tool limits of 30 seconds and 128 KiB; REST bodies over 1 MiB return 413.
MCP becomes stateless, browser calls require same-origin Origin, and chart ingress is limited to configured gateways and scrapers.
Existing authenticated clients remain supported. Refresh rejected credentials through sign-in; report access failures before rollout proceeds.
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
