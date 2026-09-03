# model-manager

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

Model management service for the Agent Platform — backend-abstracted (ollama, kserve) inventory, pull with progress, load/unload, delete and kagent ModelConfig wiring, exposed as REST and MCP

The chart deploys one Deployment that serves the REST/JSON API under `/api/v1`
(contract: `/api/v1/openapi.yaml`) and the MCP streamable-HTTP endpoint under
`/mcp`. Pick the serving backend with `backend`; the API reports the backend
and its capability flags at `GET /api/v1/backend` so clients render only what
the installation supports.

- `ollama` — proxies a host Ollama (`ollama.endpoint`, reached from pods; on
  kind the docker network gateway). Agent wiring uses kagent's native keyless
  `Ollama` provider with `ollama.agentHost` (defaults to the endpoint).
- `kserve` — InferenceServices composed from the platform's serving presets,
  the per-node Hugging Face cache (scanned by short-lived pods), pre-warm
  download Jobs with progress, Hugging Face Hub search and node fit checks.
  Agent wiring uses kagent's `OpenAI` provider against the predictor URL with
  a placeholder API-key Secret, created when the InferenceService is ready.

Used as a dependency of `agent-platform-standalone` behind a `condition`;
standalone installs set `ollama.endpoint`, `kagent.namespace`, `image.*`,
`mcp.enabled` and `muster.mcpServer.*`.

### kserve backend

`backend: kserve` needs the KServe CRDs and the platform's model-serving layer
(`components.modelServing` of `agent-platform-standalone`): the discovery
ConfigMap `agent-platform-model-serving` (`kserve.discovery.*`), the
`ServingPreset` ConfigMaps and the cache `PersistentVolumeClaim` in the serving
namespace. `kserve.namespace` must equal the platform's serving namespace: the
Role for InferenceServices, Jobs, pods and the claim is created there. Every
other `kserve.*` value is an override of what the discovery ConfigMap says and
can stay empty.

- Downloads run as Jobs in the serving namespace with the KServe
  storage-initializer image (`kserve.download.image`), into
  `<claim>/<preset name>` — the directory the preset's InferenceService mounts
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
  InferenceService exists, that its name is the directory the
  storage-initializer fills and its `hf://` storageUri the repository — so a
  directory keeps its repository and preset after its InferenceService is
  deleted. Directories whose top level holds no model (no `config.json`, no
  weights file: Hugging Face client internals such as `hf-home`, `xet`) are
  not listed as downloads.
- RBAC: a Role in the serving namespace (InferenceServices, Jobs, pods, the
  claim, `create` on ConfigMaps and `update`/`patch` on the cache index
  ConfigMap), a Role for the ConfigMaps in the discovery/preset namespace when
  it differs, and a ClusterRole for nodes and PersistentVolumes.
- Kubernetes access does not depend on wiring: the kserve driver needs the API
  for InferenceServices, Jobs, cache scans and nodes, so the pod mounts the
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
| global | object | `{}` | Platform-wide values an umbrella chart (agent-platform-standalone) shares with every component; Helm forwards them to this chart. `oauth.*` reads the identity contract as its defaults: `global.identity.issuerUrl`, `global.identity.clientId`, `global.identity.existingSecret`, `global.identity.ca.secretName` / `.key`, and `global.domain` for the OAuth base URL. Empty here; a standalone install sets `oauth.*` directly. |
| replicaCount | int | `1` | Number of replicas. Jobs are tracked in memory, so keep this at 1 unless clients pin to one pod. |
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/model-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified release name (the umbrella chart pins the Service name through this). |
| backend | string | `"ollama"` | Serving backend driver: `ollama` (host Ollama — laptop/agentlab dev loop) or `kserve` (KServe/vLLM on GPU installs: InferenceServices from serving presets, per-node Hugging Face cache, pre-warm download Jobs, fit checks). The API reports the backend and its capability flags at /api/v1/backend. |
| ollama.endpoint | string | `"http://host.docker.internal:11434"` | Ollama API base URL as reached from pods. On kind this is the docker network gateway (for example http://172.21.0.1:11434 — agentlab sets it); Docker Desktop resolves host.docker.internal. |
| ollama.agentHost | string | `""` | Ollama host written into kagent ModelConfigs, as reached by agent pods; reported as `agentEndpoint` by `GET /api/v1/backend`. Empty means the same as `ollama.endpoint`. |
| ollama.memoryBudgetGiB | int | `0` | Memory budget of the proxied host in GiB (a number; decimals allowed, also as a string so `--set` can carry them), reported as `budgetBytes` on `GET /api/v1/nodes` with `budgetSource: override` instead of `MemTotal` of the pod's `/proc/meminfo` (`host-meminfo`). Set it where the pod's view is not the host's: Docker Desktop or another VM-backed runtime (the pod sees the VM's memory), an Ollama on another machine. 0 is off; a value that is not a positive number of GiB is ignored and named in the node's `message`. The ollama counterpart of the kserve node annotation `model-manager.giantswarm.io/memory-budget-gib`. |
| kagent.namespace | string | `"kagent"` | Namespace where kagent ModelConfigs are created (RBAC is scoped here). |
| kagent.apiVersion | string | `"auto"` | kagent.dev API version for ModelConfigs; `auto` discovers the server's preferred version. |
| kagent.modelConfigPrefix | string | `""` | Prefix for generated ModelConfig names (empty: the sanitized model name, e.g. smollm2:135m -> smollm2-135m). |
| kagent.autoWire | bool | `true` | Create a ModelConfig automatically when a pull completes or a model is loaded. |
| kagent.disableWiring | bool | `false` | Disable all ModelConfig management; the `wire` capability reports false. The kserve backend keeps its Kubernetes access (ServiceAccount token, RBAC) regardless — it needs the API for InferenceServices, Jobs and nodes; only the ollama backend runs without the API when wiring is off. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint alongside the REST API. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make model-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint and the REST API require a bearer token the platform identity provider issued, and every call carries the caller's identity (logged, recorded as `requestedBy` on jobs). On the Agent Platform muster forwards the session's IdP id_token to this server (MCPServer `auth.forwardToken`, rendered below) and the portal sends the signed-in user's id_token through the gateway; both are validated against the IdP's JWKS when their audience is in `trustedAudiences`. Off: anonymous, acting as the ServiceAccount — only for a server nothing but a trusted proxy can reach. |
| oauth.baseURL | string | `""` | Public base URL of this server: the issuer of its own OAuth metadata (https, or http on loopback). Empty derives `https://<fullname>.<global.domain>` when `global.domain` is set. |
| oauth.provider | string | `"dex"` | Identity provider: `dex` or `google`. |
| oauth.dex.issuerURL | string | `""` | Dex issuer URL. Empty falls back to `global.identity.issuerUrl`. |
| oauth.dex.clientID | string | `""` | Dex OAuth client ID. Empty falls back to `global.identity.clientId`. |
| oauth.dex.clientSecret | string | `""` | Dex OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.dex.allowPrivateURLs | bool | `false` | Let the issuer resolve to a private or loopback address (an in-cluster Dex). |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Secret with the CA of a Dex that serves a private certificate; mounted and passed as `--dex-ca-file`. Empty name falls back to `global.identity.ca.secretName` / `global.identity.ca.key`. |
| oauth.google.clientID | string | `""` | Google OAuth client ID (not secret; may also come from the Secret key `google-client-id` when empty). |
| oauth.google.clientSecret | string | `""` | Google OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.existingSecret | string | `""` | Existing Secret with the provider credentials: `dex-client-secret` (dex) or `google-client-secret` (+ optional `google-client-id`) (google). Empty falls back to `global.identity.existingSecret`, whose `dex-client-secret` is the platform client's; without that, the chart renders a Secret from the values above. |
| oauth.trustedAudiences | list | `[]` | OAuth client IDs whose IdP id_tokens are accepted as bearer tokens (SSO token forwarding): the platform client muster forwards tokens for and the portal logs in with. Empty falls back to `[global.identity.clientId]`. |
| oauth.sso.allowPrivateIPs | bool | `false` | Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (an in-cluster Dex). |
| oauth.allowPublicClientRegistration | bool | `false` | Accept unauthenticated dynamic client registration (labs only). |
| oauth.downstream.enabled | bool | `false` | Call the Kubernetes API as the caller: everything a request does (InferenceServices, download Jobs, cache scans, ModelConfigs) presents the caller's IdP token, so the caller's RBAC governs — the apiserver must trust the IdP and the token's audience (a Dex install lists that audience in `muster.mcpServer.auth.requiredAudiences`; a Google install's client id is the apiserver's `--oidc-client-id`). The ServiceAccount then holds no permissions: the chart renders none of its Roles and ClusterRoles (`rbac.create` is moot), work without a caller (download adoption after a restart, the wiring reconciler) is off, and a job that outlives its caller's token fails on the apiserver's 401 instead of continuing with other credentials. |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"model-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Model management (inventory, pull, load/unload, delete, kagent wiring) for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| muster.mcpServer.auth | object | `{"forwardToken":true,"requiredAudiences":[]}` | How muster authenticates to this server; rendered only with `oauth.enabled`. `forwardToken` makes muster forward the session's IdP id_token byte-identical (the SSO path this chart trusts through `oauth.trustedAudiences`). `requiredAudiences` are extra audiences that token must carry — the Dex cross-client audience the kube-apiserver trusts (`dex-k8s-authenticator` on Giant Swarm clusters; agentlab's is `kubernetes`) so `oauth.downstream` works; muster requests them at login, so users re-login after a change. A Google IdP has no cross-client audiences: leave the list empty. |
| httpRoute.enabled | bool | `false` | Expose the service through a Gateway API HTTPRoute. |
| httpRoute.parentRefs | list | `[]` | parentRefs of the HTTPRoute (required when enabled). |
| httpRoute.hostnames | list | `[]` | Hostnames matched by the route. |
| httpRoute.annotations | object | `{}` | Annotations on the HTTPRoute. |
| httpRoute.labels | object | `{}` | Labels on the HTTPRoute. |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod. |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the API (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs (the host Ollama endpoint, e.g. 172.21.0.1/32). |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server (needed for wiring and by the kserve backend). |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the Role/RoleBinding for ModelConfigs and Secrets in `kagent.namespace` (and, when `backend` is kserve, the Role in the serving namespace, the Role for the discovery/preset ConfigMaps and the ClusterRole for nodes and PersistentVolumes) — the ServiceAccount's own permissions. Ignored with `oauth.downstream.enabled`: the ServiceAccount then gets no RBAC at all. |
| kserve.namespace | string | `"model-serving"` | Serving namespace: InferenceServices, download Jobs, cache-tool pods and the cache claim live here and the kserve RBAC Role is created here. Must match the platform's `components.modelServing.namespace.name`. |
| kserve.discovery.configMap | string | `"agent-platform-model-serving"` | Name of the platform's model-serving discovery ConfigMap (kind `ModelServingConfig`, key `config.yaml`) that carries the runtime, GPU resource name, cache claim and preset selector. Empty `kserve.*` overrides below take their value from it. |
| kserve.discovery.namespace | string | `""` | Namespace of the discovery ConfigMap and, by default, of the preset ConfigMaps. Empty means the release namespace. |
| kserve.runtime | string | `""` | ClusterServingRuntime for presets that name none; empty takes the discovery value (default `kserve-vllm`). |
| kserve.gpuResourceName | string | `""` | Accelerator resource name; empty takes the discovery value (default `nvidia.com/gpu`). |
| kserve.cache.claimName | string | `""` | PersistentVolumeClaim of the Hugging Face cache in the serving namespace; empty takes the discovery value (default `hf-cache`). |
| kserve.cache.mountPath | string | `""` | Where predictors mount the cache; empty takes the discovery value (default `/mnt/models`). |
| kserve.cache.nodes | list | `[]` | Nodes that hold the cache. Empty derives them from the node affinity of the PersistentVolume bound to the claim (a static local PV pins the cache to its node). |
| kserve.cache.indexConfigMap | string | `"model-manager-cache-index"` | ConfigMap in the serving namespace that remembers which Hugging Face repository filled which cache directory: recorded while an InferenceService serves from the directory (its name), kept after the InferenceService is gone, so the directory keeps its repository and preset in the inventory. The kserve Role grants `create` on ConfigMaps and `update`/`patch` on this name. |
| kserve.presets.namespace | string | `""` | Namespace of the serving-preset ConfigMaps; empty takes the discovery value, else `kserve.discovery.namespace`. |
| kserve.presets.labelSelector | string | `""` | Label selector of the serving-preset ConfigMaps; empty takes the discovery value (default `agent-platform.giantswarm.io/serving-preset=true`). |
| kserve.hf.endpoint | string | `"https://huggingface.co"` | Hugging Face Hub base URL (search, repository metadata, sizes). |
| kserve.hf.tokenSecret.name | string | `""` | Secret in the serving namespace holding a Hugging Face token for gated repositories (read by model-manager, mounted into download Jobs). Empty: anonymous hub access, gated models refused. |
| kserve.hf.tokenSecret.key | string | `"token"` | Key of the token in that Secret. |
| kserve.hf.token | string | `""` | Render the token Secret (`kserve.hf.tokenSecret.name`, in the serving namespace) from this value. Prefer a pre-created Secret; this is for installs whose values are already secret-managed. |
| kserve.download.image | object | `{"name":"kserve/storage-initializer","registry":"docker.io","tag":"v0.20.0"}` | Image of the pre-warm download Job. The KServe storage-initializer downloads exactly what an InferenceService would, so a later start finds the files and skips the download. |
| kserve.download.ignorePatterns | list | `[]` | File patterns downloads skip (fnmatch, passed as STORAGE_IGNORE_PATTERNS). Empty downloads the whole repository like an InferenceService does. |
| kserve.download.jobTTL | string | `"1h"` | ttlSecondsAfterFinished of download Jobs. |
| kserve.inventory.mode | string | `"pod"` | How the per-node cache contents are read. `pod`: a short-lived scan pod per cache node whenever the inventory is older than `ttl` (no long-running pods, but pod churn in the serving namespace). `daemonset`: the chart renders a DaemonSet (`kserve.inventory.agent.*`) in the serving namespace running `model-manager cache-agent`, which mounts the cache claim read-only and serves its contents over HTTP; model-manager asks the agent on the node instead of creating pods. Requires `kserve.cache.claimName` (the DaemonSet mounts the claim; the chart cannot read the discovery ConfigMap). Deletes still use a one-shot pod. |
| kserve.inventory.agent.port | int | `8081` | Port the cache agent listens on. |
| kserve.inventory.agent.nodeSelector | object | `{}` | Node selector of the DaemonSet. A node-local cache claim (static local PV, local-path) can only be mounted on its node: select it. |
| kserve.inventory.agent.tolerations | list | `[]` | Tolerations of the agent pods (GPU node taints). |
| kserve.inventory.agent.resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | Resources of the agent container (a scan walks file metadata only). |
| kserve.inventory.agent.podAnnotations | object | `{}` | Annotations on the agent pods. |
| kserve.inventory.agent.podLabels | object | `{}` | Extra labels on the agent pods. |
| kserve.inventory.image | object | `{"name":"giantswarm/alpine","registry":"gsoci.azurecr.io","tag":"3.22.1"}` | Image that creates cache directories (download Job init container) and scans / cleans the cache (short-lived pods); needs sh, find, stat, awk (busybox). |
| kserve.inventory.ttl | string | `"2m"` | How long one cache scan is reused before a node is scanned again. |
| kserve.inventory.timeout | string | `"2m"` | Time budget of one scan (scan pod, or cache-agent request). |
| kserve.budget.source | string | `"auto"` | Node memory budget for fit checks: `auto` (GPU memory from the nvidia.com/gpu.memory x gpu.count labels when present, else allocatable memory — unified-memory nodes), `gpu-labels`, `allocatable`. A node annotation `model-manager.giantswarm.io/memory-budget-gib: "96"` (GiB) overrides the budget of that node whatever the source (reported as `budgetSource: annotation`). |
| kserve.budget.defaultOverheadGiB | int | `30` | Serving overhead (KV cache, activations, runtime) added to the weights when the preset has no `requirements.overheadGiB`. |
| kserve.readyTimeout | string | `"2h"` | How long a load job waits for an InferenceService to become ready before it gives up on wiring. |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (container listens on 8080). |
| resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Container resources. |
| logging.verbose | bool | `false` | Enable debug logging. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
