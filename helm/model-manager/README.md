# model-manager

CIMD is enabled by default. DCR is deprecated by MCP 2026-07-28 and remains a
compatibility fallback; see the [OAuth registration guide](../../docs/oauth-client-registration.md).

Model management service for the Agent Platform — backend-abstracted (ollama, kserve, lemonade) inventory, pull with progress, load/unload, delete and kagent ModelConfig wiring, exposed as REST and MCP

The chart deploys one Deployment that serves the REST/JSON API under `/api/v1`
(contract: `/api/v1/openapi.yaml`) and the MCP streamable-HTTP endpoint under
`/mcp`. Pick the serving backend with `backend`, or several at once with
`backends` (one process in front of every server the host runs, e.g.
`[ollama, lemonade]`); the API reports each backend and its capability flags
at `GET /api/v1/backends` so clients render only what the installation
supports. `GET /api/v1/backend` describes the first (default) backend and
names the others.

- `ollama` — proxies a host Ollama (`ollama.endpoint`, reached from pods; on
  kind the docker network gateway). Agent wiring uses kagent's native keyless
  `Ollama` provider with `ollama.agentHost` (defaults to the endpoint).
- `lemonade` — proxies a host Lemonade Server (`lemonade.endpoint`; FastFlowLM
  on AMD Ryzen AI NPUs, llama.cpp on GPU and CPU). Agent wiring uses kagent's
  `OpenAI` provider against `lemonade.agentHost` plus `/api/v1`.
- `lmstudio` — proxies a host LM Studio (`lmstudio.endpoint`, 0.4.0 or newer;
  llama.cpp on GPU and CPU, MLX on Apple silicon). Agent wiring uses kagent's
  `OpenAI` provider against `lmstudio.agentHost` plus `/v1`. It serves no
  delete: that call answers `501 unsupported`, and a model is removed with
  `lms rm` on the host.
- `kserve` — LLMInferenceServices composed from the platform's serving presets,
  the per-node Hugging Face cache (scanned by short-lived pods), pre-warm
  download Jobs with progress, Hugging Face Hub search and node fit checks.
  Agent wiring uses kagent's `OpenAI` provider against the predictor URL with
  a placeholder API-key Secret, created when the LLMInferenceService is ready.

A component of the [`giantswarm/agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart (`components.model-manager.enabled`), which sets `backend`, the
backend endpoints (`ollama.*`, `lemonade.*`, `lmstudio.*`), `kagent.namespace`,
`mcp.enabled`, `oauth.*` and `muster.mcpServer.*`.

### Several backends at once

`backends: [ollama, lemonade]` runs both drivers in one process: the Deployment
passes `--backends` and the flags of every listed driver (each reads its own
block — `ollama.*`, `lemonade.*`, `lmstudio.*`, `kserve.*`), the ServiceAccount token, the
kserve Roles, the cache-agent DaemonSet and the Hugging Face token Secret
render when kserve is listed. The order is the operator's: the first backend
is the **default backend** — the one `GET /api/v1/backend` describes and the
one `POST /api/v1/models/pull` uses when the request names none (a pull names a
model that does not exist yet, so nothing else can pick). Every model, loaded
entry, job, node and ModelConfig reference carries `backend`; reads take
`?backend=`, mutations a `backend` field, and an unqualified reference that
exists on two backends answers `409 conflict`. `backend: ollama` alone renders
exactly what it always did.

### kserve backend

`backend: kserve` needs the KServe CRDs and the platform's model-serving layer
(`components.modelServing` of the `agent-platform` meta chart, rendered by its
connectivity chart): the discovery
ConfigMap `agent-platform-model-serving` (`kserve.discovery.*`), the
`ServingPreset` ConfigMaps and the cache `PersistentVolumeClaim` in the serving
namespace. `kserve.namespace` must equal the platform's serving namespace: the
Role for LLMInferenceServices, Jobs, pods and the claim is created there. Every
other `kserve.*` value is an override of what the discovery ConfigMap says and
can stay empty.

- Downloads run as Jobs in the serving namespace with the KServe
  storage-initializer image (`kserve.download.image`), into
  `<claim>/<preset name>` — the directory the preset's LLMInferenceService mounts
  — after a fit check against the node's memory budget (`kserve.budget.*`;
  the node annotation `model-manager.giantswarm.io/memory-budget-gib: "96"`
  overrides one node's budget, e.g. a unified-memory node without GPU memory
  labels). Gated repositories need `kserve.hf.tokenSecret` (a Secret in the
  serving namespace).
- The cache inventory (`kserve.inventory.*`) reads each cache node either with
  a short-lived scan pod (`mode: pod`, the default; results are reused for
  `kserve.inventory.ttl`) or, with `mode: daemonset`, through a DaemonSet in
  the serving namespace that runs `model-manager cache-agent` — the same
  image, mounting the claim read-only and serving its contents over HTTP — so
  no pods are created per read. Daemonset mode needs `kserve.cache.claimName`
  and usually `kserve.inventory.agent.nodeSelector` (a node-local claim mounts
  on its node only); `networkPolicy.enabled` then also allows model-manager →
  agent traffic. Deletes keep using a one-shot pod.
- The inventory names a cache directory after the repository known to have
  filled it: the marker of a pre-warm download, else the **cache index** — the
  ConfigMap `kserve.cache.indexConfigMap` (default `model-manager-cache-index`)
  in the serving namespace, in which model-manager records, while an
  LLMInferenceService exists, that its name is the directory the
  storage-initializer fills and its `hf://` storageUri the repository — so a
  directory keeps its repository and preset after its LLMInferenceService is
  deleted. Directories whose top level holds no model (no `config.json`, no
  weights file: Hugging Face client internals such as `hf-home`, `xet`) are
  not listed as downloads.
- RBAC: a Role in the serving namespace (LLMInferenceServices, Jobs, pods, the
  claim, `create` on ConfigMaps and `update`/`patch` on the cache index
  ConfigMap), a Role for the ConfigMaps in the discovery/preset namespace when
  it differs, and a ClusterRole for nodes and PersistentVolumes.
- Kubernetes access does not depend on wiring: the kserve driver needs the API
  for LLMInferenceServices, Jobs, cache scans and nodes, so the pod mounts the
  ServiceAccount token and the RBAC above renders even with
  `kagent.disableWiring: true` — that value only removes the ModelConfig
  management and its Role in `kagent.namespace`. Only the ollama backend runs
  without the API when wiring is off.

**Homepage:** <https://github.com/giantswarm/model-manager>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Giant Swarm | <team-bumblebee@giantswarm.io> |  |

## Source Code

* <https://github.com/giantswarm/model-manager>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| global | object | `{}` | Platform-wide values a parent chart (the `agent-platform` meta chart) shares with every component. `global.domain` derives the OAuth issuer. `global.identity` is the platform's sign-in, the fallback for `oauth.authorizationServer.upstream` and the derived trusted issuer: `issuerUrl`, `clientId`, `existingSecret` (`dex-client-secret`), `ca` (`secretName`, `key`), `type` (`dex` or `google`) and `kubernetes.audiences`, the audiences the Kubernetes API server requires (its `AuthenticationConfiguration` `jwt.issuer.audiences`). Empty here; a chart installed on its own sets `oauth.*` directly. |
| replicaCount | int | `1` | Number of replicas. Jobs are tracked in memory, so keep this at 1 unless clients pin to one pod. |
| revisionHistoryLimit | int | `3` | Old ReplicaSets the Deployment keeps after a rollout. Kubernetes' default of ten leaves stale ReplicaSets behind that policy reports keep flagging. |
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/model-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified release name (the umbrella chart pins the Service name through this). |
| backend | string | `""` | Serving backend driver: `ollama` (host Ollama — laptop/agentlab dev loop), `kserve` (KServe/llm-d on GPU installs: LLMInferenceServices from serving presets, per-node Hugging Face cache, pre-warm download Jobs, fit checks) or `lemonade` (a Lemonade Server on the host — FastFlowLM on AMD Ryzen AI NPUs, llama.cpp on GPU and CPU; the same proxying shape as ollama) or `lmstudio` (an LM Studio on the host — llama.cpp on GPU and CPU, MLX on Apple silicon; proxied like ollama, but it serves no delete). The API reports the backend and its capability flags at /api/v1/backend. The one-backend form of `backends`. Empty (the default), like an empty `backends`, starts with no static backend: backends are then registered at runtime with the `add_backend` tool as backend documents — ConfigMaps in the release namespace labelled `agent-platform.giantswarm.io/model-backend=true` — and listed with `source: person` or `cluster-manager` (see docs/backends.md); a static value here is listed as `source: static` and cannot be registered again at runtime. |
| backends | list | `[]` | Serving backends to run at once, in the operator's order — one process in front of several servers, for example `[ollama, lemonade]` on a host running both. Each driver at most once; every listed driver reads its own block (`ollama.*`, `lemonade.*`, `lmstudio.*`, `kserve.*`). The first is the default backend: the one `GET /api/v1/backend` describes and an unqualified pull goes to; every model, job and node the API returns names its backend, every request may name one (`GET /api/v1/backends` lists them). Empty runs `backend` alone — and with `backend` empty too, no static backend at all. |
| ollama.endpoint | string | `"http://host.docker.internal:11434"` | Ollama API base URL as reached from pods. On kind this is the docker network gateway (for example http://172.21.0.1:11434 — agentlab sets it); Docker Desktop resolves host.docker.internal. |
| ollama.agentHost | string | `""` | Ollama host written into kagent ModelConfigs, as reached by agent pods; reported as `agentEndpoint` by `GET /api/v1/backend`. Empty means the same as `ollama.endpoint`. |
| ollama.memoryBudgetGiB | int | `0` | Memory budget of the proxied host in GiB (a number; decimals allowed, also as a string so `--set` can carry them), reported as `budgetBytes` on `GET /api/v1/nodes` with `budgetSource: override` instead of `MemTotal` of the pod's `/proc/meminfo` (`host-meminfo`). Set it where the pod's view is not the host's: Docker Desktop or another VM-backed runtime (the pod sees the VM's memory), an Ollama on another machine. 0 is off; a value that is not a positive number of GiB is ignored and named in the node's `message`. The ollama counterpart of the kserve node annotation `model-manager.giantswarm.io/memory-budget-gib`. |
| ollama.contextLength | int | `32768` | Context window in tokens that agents run Ollama models at: written into every Ollama ModelConfig model-manager creates or re-wires as `spec.ollama.options.num_ctx`, used by loads, and capped at each model's own context length; reported as `modelConfig.contextLength`. Without it Ollama picks its default from the host's VRAM (4096 tokens below 24 GiB, which is every CPU-only host, 32768 up to 48 GiB, 262144 above) and a longer prompt silently loses its front: the agent's system prompt and tool schemas. Ollama reserves the KV cache for the whole window in the host's memory when it loads a model, for example about 2.5 GiB at 32768 for a 3B model that keeps 80 KiB per token; lower it on a host short of memory. 0 writes none and leaves the window to the server (`OLLAMA_CONTEXT_LENGTH` on the host). |
| ollama.think | bool | `false` | Think field that agents send to Ollama models with the thinking capability (Qwen3.5, Qwen3, DeepSeek-R1, Granite 4.2, ...): written into their ModelConfigs as `spec.ollama.think` whenever model-manager creates or re-wires them, and reported as `modelConfig.think`. `false` has them answer directly. `true` has them reason before every answer. `""` writes none and leaves Ollama's default, under which such a model thinks. Thinking costs 1.5–2k tokens per model call, minutes on a CPU, and a small model such as `qwen3.5:2b` can put its whole answer into the thinking, which the agent runtime does not read, so the agent replies with nothing. Models without the capability get none, since Ollama refuses `true` for them. Needs kagent 1.0.3 or newer; on an older kagent, whose ModelConfig has no `think`, none is written. |
| lemonade.endpoint | string | `"http://host.docker.internal:13305"` | Lemonade Server base URL as reached from pods (its API is under `/api/v1`; Lemonade listens on 13305 by default). Bind Lemonade to every interface (`lemonade config set host=0.0.0.0`, or `host` in its config.json) so the kind docker network gateway reaches it, for example http://172.21.0.1:13305; Docker Desktop resolves host.docker.internal. |
| lemonade.agentHost | string | `""` | Lemonade Server base URL as reached by agent pods, written into kagent ModelConfigs as the OpenAI-compatible `openAI.baseUrl` with `/api/v1` appended; reported as `agentEndpoint` by `GET /api/v1/backend`. Empty means the same as `lemonade.endpoint`. |
| lmstudio.endpoint | string | `"http://host.docker.internal:1234"` | LM Studio base URL as reached from pods (its API is under `/api/v1`; LM Studio listens on 1234 by default). Serve it on the local network rather than loopback — `LMS_SERVER_HOST=0.0.0.0`, or the app's "Serve on Local Network" toggle — so the kind docker network gateway reaches it, for example http://172.21.0.1:1234; Docker Desktop resolves host.docker.internal. LM Studio 0.4.0 or newer is required — the `/api/v1` API is what this driver speaks. |
| lmstudio.agentHost | string | `""` | LM Studio base URL as reached by agent pods, written into kagent ModelConfigs as the OpenAI-compatible `openAI.baseUrl` with `/v1` appended; reported as `agentEndpoint` by `GET /api/v1/backend`. Empty means the same as `lmstudio.endpoint`. |
| kagent.namespace | string | `"kagent"` | Namespace where kagent ModelConfigs are created (RBAC is scoped here). |
| kagent.apiVersion | string | `"auto"` | ModelConfig API version. `auto` discovers group and version; a bare version such as `v1alpha3` discovers the group serving it; a group/version such as `api.kagent.dev/v1alpha3` pins both. Discovery prefers `api.kagent.dev` (kagent 1.3 and later) over `kagent.dev` and re-runs every minute and when a call fails NotFound, so a running process follows a kagent upgrade. |
| kagent.modelConfigPrefix | string | `""` | Prefix for generated ModelConfig names (empty: the sanitized model name, e.g. smollm2:135m -> smollm2-135m). |
| kagent.autoWire | bool | `true` | Create a ModelConfig automatically when a pull completes or a model is loaded. |
| kagent.disableWiring | bool | `false` | Disable all ModelConfig management; the `wire` capability reports false. A release that runs the kserve backend keeps its Kubernetes access (ServiceAccount token, RBAC) regardless — it needs the API for LLMInferenceServices, Jobs and nodes; only releases with the ollama and lemonade backends alone run without the API when wiring is off. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint alongside the REST API. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make model-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint and the REST API require a bearer token, and every call carries the caller's identity (logged, recorded as `requestedBy` on jobs). On the Agent Platform muster forwards the session's IdP ID token to this server (MCPServer `auth.forwardToken`, rendered below) and the portal sends the signed-in user's ID token through the gateway; both are accepted from the provider people sign in with (`resourceServer.trustedIssuers`). Off: anonymous, acting as the ServiceAccount, only for a server nothing but a trusted proxy can reach. |
| oauth.resourceServer.resource | string | `""` | The main protected resource, by its full URL (`OAUTH_RESOURCE`). Empty uses the issuer. |
| oauth.resourceServer.additionalResources | list | `[]` | Further protected resources on the same origin, each by its full URL (`OAUTH_ADDITIONAL_RESOURCES`). The REST API and the MCP endpoint are always protected resources. |
| oauth.resourceServer.authorizationServers | list | `[]` | Issuer identifiers the resource metadata advertises (`OAUTH_AUTHORIZATION_SERVERS`, RFC 9728 `authorization_servers`): this server's issuer or a trusted issuer. Empty advertises this server's issuer; required with `authorizationServer.enabled` false, since MCP clients find the authorization server here. |
| oauth.resourceServer.trustedIssuers | list | `[]` | The issuers whose JWTs are accepted (`OAUTH_TRUSTED_ISSUERS`). A trust anchor: set it only from the release's own values or a GitOps-managed ConfigMap that only installation administrators write. Each entry: `issuer`, `jwksURI` and `audiences` (required), `subjectClaim` (`sub` or `email`), `acceptedTypHeaders`, `allowedClaims`, `allowPrivateIPJWKSHosts`, `openIDProvider` (true for the provider people sign in with, whose bearers are their ID tokens), and `certificateAuthority` with a `secretKeyRef` or `configMapKeyRef` (`name`, `key`). Empty trusts the provider people sign in with (`authorizationServer.upstream`) as `openIDProvider`: its ID tokens are accepted as bearer tokens, a deviation from MCP's audience rule until per-backend tokens (ID-JAG, giantswarm/giantswarm#39102). They are accepted for the client people sign in with (`authorizationServer.upstream.client.id`, or the deprecated `oauth.trustedAudiences`) and `muster.mcpServer.auth.requiredAudiences`, in that order and without duplicates. |
| oauth.resourceServer.requireResourceAccessTokens | bool | `false` | Accept only tokens whose audience names this server's resource (`OAUTH_REQUIRE_RESOURCE_ACCESS_TOKENS`), as MCP requires. Off while the platform forwards ID tokens (giantswarm/giantswarm#39102). |
| oauth.authorizationServer.enabled | bool | `true` | Run the authorization server. Off makes model-manager a resource server only (`OAUTH_ENABLE_AUTHORIZATION_SERVER=false`). |
| oauth.authorizationServer.issuer | string | `""` | Public base URL of this server, the issuer of its tokens and metadata (`OAUTH_ISSUER`; https, or http on loopback). Empty uses the deprecated `oauth.baseURL`, else `https://<fullname>.<global.domain>`. |
| oauth.authorizationServer.upstream.type | string | `""` | The OpenID Provider people sign in with: `dex` or `google` (`OAUTH_PROVIDER`). Empty uses the deprecated `oauth.provider`, else `dex`. |
| oauth.authorizationServer.upstream.issuer | string | `""` | Dex issuer URL. Empty uses the deprecated `oauth.dex.issuerURL`, else `global.identity.issuerUrl`. |
| oauth.authorizationServer.upstream.client.id | string | `""` | OAuth client ID at the provider. Empty uses the deprecated `oauth.dex.clientID` or `oauth.google.clientID`, else `global.identity.clientId` (dex). |
| oauth.authorizationServer.upstream.client.secretKeyRef | object | `{"key":"client-secret","name":""}` | Secret key with the client secret, mounted as a file. Empty name uses the deprecated `oauth.existingSecret`, else `global.identity.existingSecret`, else a Secret from the deprecated inline client secret. |
| oauth.authorizationServer.upstream.certificateAuthority.secretKeyRef | object | `{"key":"ca.crt","name":""}` | Secret key with the CA of a Dex that serves a private certificate (`OAUTH_DEX_CA_FILE`). Empty name uses the deprecated `oauth.dex.caSecret`, else `global.identity.ca`. |
| oauth.authorizationServer.dynamicClientRegistration.enabled | bool | `true` | Serve RFC 7591 dynamic client registration (`OAUTH_ENABLE_DYNAMIC_CLIENT_REGISTRATION`). MCP 2026-07-28 deprecates it in favour of client ID metadata documents; it stays on for clients that still register this way. |
| oauth.authorizationServer.dynamicClientRegistration.initialAccessToken.secretKeyRef | object | `{"key":"token","name":""}` | Secret key with the RFC 7591 initial access token a registration must present (mcp-oauth reads it from `OAUTH_REGISTRATION_ACCESS_TOKEN_FILE`). |
| oauth.authorizationServer.clientIDMetadataDocuments.enabled | bool | `true` | Accept URL client IDs (client ID metadata documents) (`OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS`). |
| oauth.authorizationServer.storage.type | string | `"memory"` | Token storage (`OAUTH_STORAGE_BACKEND`). Memory loses every session on restart and needs one replica. |
| oauth.baseURL | string | `""` | Deprecated: use `authorizationServer.issuer`. |
| oauth.provider | string | `""` | Deprecated: use `authorizationServer.upstream.type`. |
| oauth.dex.issuerURL | string | `""` | Deprecated: use `authorizationServer.upstream.issuer`. |
| oauth.dex.clientID | string | `""` | Deprecated: use `authorizationServer.upstream.client.id`. |
| oauth.dex.clientSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. |
| oauth.dex.allowPrivateURLs | bool | `false` | Deprecated, no effect, not passed to the server: the Dex client reaches private addresses and follows HTTP_PROXY, HTTPS_PROXY and NO_PROXY without it. Remove it from your values; the key goes away in a later release. |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Deprecated: use `authorizationServer.upstream.certificateAuthority`. |
| oauth.google.clientID | string | `""` | Deprecated: use `authorizationServer.upstream.client.id`. Empty reads the Secret key `google-client-id`. |
| oauth.google.clientSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. |
| oauth.existingSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. Existing Secret with `dex-client-secret` (dex) or `google-client-secret` (+ optional `google-client-id`) (google). |
| oauth.trustedAudiences | list | `[]` | Deprecated: list the provider in `resourceServer.trustedIssuers` with these `audiences`. The OAuth client IDs whose ID tokens of the provider people sign in with are accepted. |
| oauth.sso.allowPrivateIPs | bool | `false` | Deprecated, no effect, not passed to the server: the issuer host of the configured provider may resolve to a private address for its JWKS without it; list any other JWKS host in allowPrivateIPJWKSHosts. Remove it from your values; the key goes away in a later release. |
| oauth.sso.allowPrivateIPJWKSHosts | list | `[]` | Deprecated: list the provider in `resourceServer.trustedIssuers` with these `allowPrivateIPJWKSHosts`. Hosts, other than the issuer's own, whose JWKS may resolve to an RFC 1918 or RFC 4193 address. |
| oauth.allowPublicClientRegistration | bool | `false` | Deprecated, removed in a later release: accept dynamic client registration without an initial access token, public clients included (labs only). MCP clients register with client ID metadata documents. |
| oauth.downstream.enabled | bool | `false` | Call the Kubernetes API as the caller: everything a request does (LLMInferenceServices, download Jobs, cache scans, ModelConfigs) presents the caller's IdP token, so the caller's RBAC governs — the apiserver must trust the IdP and the token's audience (a Dex install lists that audience in `muster.mcpServer.auth.requiredAudiences`; a Google install's client id is the apiserver's `--oidc-client-id`). The ServiceAccount then holds no permissions: the chart renders none of its Roles and ClusterRoles (`rbac.create` is moot), work without a caller (download adoption after a restart, the wiring reconciler) is off, and a job that outlives its caller's token fails on the apiserver's 401 instead of continuing with other credentials. Presenting the forwarded token is token passthrough, which MCP forbids: a deviation until per-backend tokens (ID-JAG, giantswarm/giantswarm#39102). |
| github.enabled | bool | `false` | Offer commit mode (`mode: commit` on `wire_model` and `unwire_model`: the pull request opened as the person in the repository that owns the kagent namespace). Needs `oauth.enabled` and `tokenExchange`: the MCPServer forwards the person's ID token as without commit mode and model-manager fetches their GitHub App user grant per commit. Off: muster forwards the ID token as the bearer (`muster.mcpServer.auth.forwardToken`) and commit mode is refused. |
| github.apiURL | string | `"https://api.github.com"` | GitHub's REST API, for `GET /user` and the pull request. |
| github.tokenExchange.tokenEndpoint | string | `""` | RFC 8693 token endpoint (muster's broker) that releases the person's GitHub App user grant for a commit: only commit mode asks for GitHub, and a commit without the grant answers `grant_required` naming `audience`. Needs `github.enabled`, `oauth.enabled` with Dex, `oauth.downstream.enabled` and `muster.mcpServer.auth.forwardToken`. With `auth.providerApps`, muster also attaches the person's App token to every call until giantswarm/giantswarm#39227. Required with `github.enabled`. |
| github.tokenExchange.audience | string | `""` | RFC 8693 audience: muster's name for the provider App whose grant the broker releases (its `aggregator.providerApps` key), installed with contents and pull-request write on every repository a commit targets. Also the App named in `grant_required` and the MCPServer's `auth.providerApps`, so muster answers with that App's sign-in link. Required with `tokenEndpoint`. |
| github.tokenExchange.clientID | string | `""` | Confidential broker client ID, allowed to retrieve only that App grant. Empty: read from the Secret's `client-id` key. |
| github.tokenExchange.clientSecret | string | `""` | Confidential broker client secret, rendered into the `<fullname>-github-token-exchange` Secret when `existingSecret` is empty. Prefer `existingSecret`. |
| github.tokenExchange.existingSecret | string | `""` | Existing Secret in the release namespace with `client-secret` (and `client-id` when `clientID` is empty); mounted whole. |
| github.tokenExchange.caSecretName | string | `""` | Optional existing Secret with `ca.crt` for the broker's private CA. |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"model-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.displayName | string | `"Model Manager"` | Human-friendly name the Dev Portal shows for this server, rendered as the `ui.giantswarm.io/display-name` annotation. Empty renders none. |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Model management (inventory, pull, load/unload, delete, kagent wiring) for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| muster.mcpServer.auth | object | `{"forwardToken":true,"requiredAudiences":[]}` | How muster authenticates to this server; rendered only with `oauth.enabled`. `forwardToken` makes muster forward the session's IdP ID token byte-identical, accepted through the OpenID Provider's trusted issuer entry. `requiredAudiences` are extra audiences that token must carry — the Dex cross-client audience the kube-apiserver trusts (`dex-k8s-authenticator` on Giant Swarm clusters; agentlab's is `kubernetes`) so `oauth.downstream` works; muster requests them at login, so users re-login after a change. The derived trusted issuer entry accepts them, since its audiences are the platform client plus this list. Empty uses `global.identity.kubernetes.audiences`. A Google IdP has no cross-client audiences: leave both empty. |
| httpRoute.enabled | bool | `false` | Expose the service through a Gateway API HTTPRoute. |
| httpRoute.parentRefs | list | `[]` | parentRefs of the HTTPRoute (required when enabled). |
| httpRoute.hostnames | list | `[]` | Hostnames matched by the route. |
| httpRoute.annotations | object | `{}` | Annotations on the HTTPRoute. |
| httpRoute.labels | object | `{}` | Labels on the HTTPRoute. |
| networkPolicy.enabled | bool | `true` | Create a Kubernetes NetworkPolicy for the pod. |
| networkPolicy.gatewayPeers | list | `[{"podSelector":{"matchLabels":{"app.kubernetes.io/name":"muster"}}},{"podSelector":{"matchLabels":{"app.kubernetes.io/name":"agentgateway"}}}]` | NetworkPolicy peers allowed to reach the API. Pod selectors alone match the release namespace. Configure the actual gateway data-plane selector when using an HTTPRoute with another gateway implementation. |
| networkPolicy.ingressNamespaces | list | `[]` | Legacy additional namespaces allowed to reach the API. Each explicitly configured namespace allows all its pods (label kubernetes.io/metadata.name). |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs (the host Ollama, Lemonade or LM Studio endpoint, e.g. 172.21.0.1/32). |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server (needed for wiring and by the kserve backend). |
| networkPolicy.metricsPeers | list | `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}},"podSelector":{"matchLabels":{"app.kubernetes.io/instance":"alloy-metrics","app.kubernetes.io/name":"alloy"}}}]` | NetworkPolicy peers allowed to scrape metrics when enabled. |
| networkPolicy.metricsNamespaces | list | `[]` | Legacy additional namespaces allowed to scrape metrics. Each explicitly configured namespace allows all its pods. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the Role/RoleBinding for ModelConfigs and Secrets in `kagent.namespace` (and, when kserve is among the backends, the Role in the serving namespace, the Role for the discovery/preset ConfigMaps and the ClusterRole for nodes and PersistentVolumes) — the ServiceAccount's own permissions. Ignored with `oauth.downstream.enabled`: the ServiceAccount then gets no RBAC at all. |
| kserve.namespace | string | `"model-serving"` | Serving namespace: LLMInferenceServices, download Jobs, cache-tool pods and the cache claim live here and the kserve RBAC Role is created here. Must match the platform's `components.modelServing.namespace.name`. |
| kserve.discovery.configMap | string | `"agent-platform-model-serving"` | Name of the platform's model-serving discovery ConfigMap (kind `ModelServingConfig`, key `config.yaml`) that carries the runtime, GPU resource name, cache claim and preset selector. Empty `kserve.*` overrides below take their value from it. |
| kserve.discovery.namespace | string | `""` | Namespace of the discovery ConfigMap and, by default, of the preset ConfigMaps. Empty means the release namespace. |
| kserve.gpuResourceName | string | `""` | Accelerator resource name; empty takes the discovery value (default `nvidia.com/gpu`). |
| kserve.cache.claimName | string | `""` | PersistentVolumeClaim of the Hugging Face cache in the serving namespace; empty takes the discovery value (default `hf-cache`). |
| kserve.cache.mountPath | string | `""` | Where predictors mount the cache; empty takes the discovery value (default `/mnt/models`). |
| kserve.cache.nodes | list | `[]` | Nodes that hold the cache. Empty derives them from the node affinity of the PersistentVolume bound to the claim (a static local PV pins the cache to its node). |
| kserve.cache.indexConfigMap | string | `"model-manager-cache-index"` | ConfigMap in the serving namespace that remembers which Hugging Face repository filled which cache directory: recorded while an LLMInferenceService serves from the directory (its name), kept after the LLMInferenceService is gone, so the directory keeps its repository and preset in the inventory. The kserve Role grants `create` on ConfigMaps and `update`/`patch`/`delete` on this name. |
| kserve.presets.namespace | string | `""` | Namespace of the serving-preset ConfigMaps; empty takes the discovery value, else `kserve.discovery.namespace`. |
| kserve.presets.labelSelector | string | `""` | Label selector of the serving-preset ConfigMaps; empty takes the discovery value (default `agent-platform.giantswarm.io/serving-preset=true`). |
| kserve.hf.endpoint | string | `"https://huggingface.co"` | Hugging Face Hub base URL (search, repository metadata, sizes). |
| kserve.hf.timeout | string | `"4s"` | Time budget of the Hugging Face Hub lookups of one fit check (repository metadata, file tree, safetensors index) or search. The hub answers in well under a second when reachable; when it does not answer (egress blocked), `check_fit` falls back to the preset's requirements within this budget, so keep it below the caller's meta-tool deadline (muster: 10s). Download Jobs are not affected. |
| kserve.hf.tokenSecret.name | string | `""` | Secret in the serving namespace holding a Hugging Face token for gated repositories (read by model-manager, mounted into download Jobs). Empty: anonymous hub access, gated models refused. |
| kserve.hf.tokenSecret.key | string | `"token"` | Key of the token in that Secret. |
| kserve.hf.token | string | `""` | Render the token Secret (`kserve.hf.tokenSecret.name`, in the serving namespace) from this value. Prefer a pre-created Secret; this is for installs whose values are already secret-managed. |
| kserve.download.image | object | `{"name":"giantswarm/storage-initializer","registry":"gsoci.azurecr.io","tag":"v0.20.0"}` | Image of the pre-warm download Job. The KServe storage-initializer downloads exactly what an LLMInferenceService would, so a later start finds the files and skips the download. |
| kserve.download.ignorePatterns | list | `[]` | File patterns downloads skip (fnmatch, passed as STORAGE_IGNORE_PATTERNS). Empty downloads the whole repository like an LLMInferenceService does. |
| kserve.download.jobTTL | string | `"1h"` | ttlSecondsAfterFinished of download Jobs. |
| kserve.download.stallTimeout | string | `"10m"` | A download Job that writes nothing to its target directory for this long fails with a reason the pull job shows (`DOWNLOAD STALLED: no bytes written to <dir> for <N>s`) instead of hanging; partial files stay for a retry. The Job downloads with `HF_HUB_DISABLE_XET=1` and `HF_HUB_ENABLE_HF_TRANSFER=1`: the Xet client connects to CDN addresses a Cilium `toFQDNs` DNS proxy never resolved and hangs on the dropped SYNs, hf_transfer resolves per connection through the system resolver. |
| kserve.download.ephemeralStorage | object | `{"limit":"1Gi","request":"64Mi"}` | ephemeral-storage of the download Job's container: the request, and the limit, which is also the sizeLimit of its `tmp` emptyDir (the hub client's HF_HOME). The weights land on the cache claim, not here. |
| kserve.inventory.mode | string | `"pod"` | How the per-node cache contents are read. `pod`: a short-lived scan pod per cache node whenever the inventory is older than `ttl` (no long-running pods, but pod churn in the serving namespace). `daemonset`: the chart renders a DaemonSet (`kserve.inventory.agent.*`) in the serving namespace running `model-manager cache-agent`, which mounts the cache claim read-only and serves its contents over HTTP; model-manager asks the agent on the node instead of creating pods. Requires `kserve.cache.claimName` (the DaemonSet mounts the claim; the chart cannot read the discovery ConfigMap). Deletes still use a one-shot pod. |
| kserve.inventory.agent.port | int | `8081` | Port the cache agent listens on. |
| kserve.inventory.agent.nodeSelector | object | `{}` | Node selector of the DaemonSet. A node-local cache claim (static local PV, local-path) can only be mounted on its node: select it. |
| kserve.inventory.agent.tolerations | list | `[]` | Tolerations of the agent pods (GPU node taints). |
| kserve.inventory.agent.resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | Resources of the agent container (a scan walks file metadata only). |
| kserve.inventory.agent.podAnnotations | object | `{}` | Annotations on the agent pods. |
| kserve.inventory.agent.podLabels | object | `{}` | Extra labels on the agent pods. |
| kserve.inventory.image | object | `{"name":"giantswarm/alpine","registry":"gsoci.azurecr.io","tag":"3.22.1"}` | Image that creates cache directories (download Job init container) and scans / cleans the cache (short-lived Jobs); needs sh, find, stat, awk (busybox). |
| kserve.inventory.ttl | string | `"2m"` | How long one cache scan is reused before a node is scanned again. |
| kserve.inventory.timeout | string | `"2m"` | Time budget of one scan (scan Job, or cache-agent request); a caller whose deadline leaves less is answered from the last scan while a new one runs in the background. |
| kserve.budget.source | string | `"auto"` | Node memory budget for fit checks: `auto` (GPU memory from the nvidia.com/gpu.memory x gpu.count labels when present, else allocatable memory — unified-memory nodes), `gpu-labels`, `allocatable`. A node annotation `model-manager.giantswarm.io/memory-budget-gib: "96"` (GiB) overrides the budget of that node whatever the source (reported as `budgetSource: annotation`). |
| kserve.budget.defaultOverheadGiB | int | `30` | Serving overhead (KV cache, activations, runtime) added to the weights when the preset has no `requirements.overheadGiB`. |
| kserve.budget.unifiedHostHeadroomGiB | int | `16` | GiB a unified-memory GPU node, one whose GPUs have no memory of their own like a GB10, keeps for its host beside what vLLM claims at start, the GPU memory utilization share of the node's whole memory. A preset whose claim leaves less is refused. |
| kserve.readyTimeout | string | `"2h"` | How long a load job waits for an LLMInferenceService to become ready before it gives up on wiring. |
| kserve.scaleUpTimeout | string | `"10m"` | The GPU pool's scale-up budget: how long a predictor may wait for a node while Karpenter refuses to launch one (`InsufficientInstanceCapacity`) before its `scheduling` step and the phase read `failed` naming the refusal (`CapacityUnavailable`), counted from the pod's creation. |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (container listens on 8080). |
| resources | object | `{"limits":{"cpu":"500m","ephemeral-storage":"128Mi","memory":"256Mi"},"requests":{"cpu":"50m","ephemeral-storage":"32Mi","memory":"64Mi"}}` | Container resources. ephemeral-storage covers the `tmp` emptyDir mounted at /tmp, which holds only the Go runtime's scratch files. |
| tmpVolume.sizeLimit | string | `"128Mi"` | Size limit of the `tmp` emptyDir mounted at /tmp; the kubelet evicts the pod when the volume grows past it. Empty sets no limit. |
| logging.verbose | bool | `false` | Enable debug logging. |
| observability.otel.endpoint | string | `""` | OTLP endpoint traces are exported to, e.g. `http://otlp-gateway.kube-system.svc:4317`. Empty exports nothing; the W3C propagator still carries an inbound traceparent. |
| observability.otel.protocol | string | `"grpc"` | OTLP protocol. |
| observability.otel.headers | string | `""` | OTEL_EXPORTER_OTLP_HEADERS, e.g. `X-Scope-OrgID=giantswarm` for a multi-tenant collector. |
| observability.otel.resourceAttributes | string | `""` | Appended to the downward-API k8s.namespace.name, k8s.pod.name and k8s.node.name in OTEL_RESOURCE_ATTRIBUTES, e.g. `deployment.environment=glean`. |
| observability.otel.sampler | string | `"parentbased_traceidratio"` | OTEL_TRACES_SAMPLER. The parent-based samplers follow the caller's sampling decision. |
| observability.otel.samplerArg | string | `"0.1"` | OTEL_TRACES_SAMPLER_ARG: the ratio of root traces sampled. |
| observability.metrics.enabled | bool | `true` | Serve Prometheus metrics on `GET /metrics` of the `metrics` port: `mcp_server_operation_duration_seconds` per tool, and `http_server_request_duration_seconds` for the REST API and the MCP endpoint. Off, `OTEL_METRICS_EXPORTER=none`: nothing is served, and nothing is pushed to `observability.otel.endpoint` either. |
| observability.metrics.port | int | `9464` | Port of the metrics listener, apart from the API port so an HTTPRoute or the OAuth guard never covers it. |
| serviceMonitor.enabled | bool | `false` | Render a ServiceMonitor for the `metrics` port. Also needs `observability.metrics.enabled`; the meta chart turns this on with the tenant label. |
| serviceMonitor.interval | string | `""` | Scrape interval; empty uses the Prometheus Operator default. |
| serviceMonitor.labels | object | `{"observability.giantswarm.io/tenant":"giantswarm"}` | Labels on the ServiceMonitor, beside the chart's own. The Giant Swarm observability platform routes a scrape to a Mimir tenant by `observability.giantswarm.io/tenant`; a monitor without it writes to no tenant. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
