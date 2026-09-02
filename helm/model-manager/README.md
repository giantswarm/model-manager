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
- The cache inventory runs a short-lived pod per cache node
  (`kserve.inventory.*`); results are reused for `kserve.inventory.ttl`.
- RBAC: a Role in the serving namespace, a Role for the ConfigMaps in the
  discovery/preset namespace when it differs, and a ClusterRole for nodes and
  PersistentVolumes.

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
| kagent.namespace | string | `"kagent"` | Namespace where kagent ModelConfigs are created (RBAC is scoped here). |
| kagent.apiVersion | string | `"auto"` | kagent.dev API version for ModelConfigs; `auto` discovers the server's preferred version. |
| kagent.modelConfigPrefix | string | `""` | Prefix for generated ModelConfig names (empty: the sanitized model name, e.g. smollm2:135m -> smollm2-135m). |
| kagent.autoWire | bool | `true` | Create a ModelConfig automatically when a pull completes or a model is loaded. |
| kagent.disableWiring | bool | `false` | Disable all ModelConfig management; the `wire` capability reports false. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint alongside the REST API. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Protect the MCP endpoint with an embedded OAuth 2.1 server (Dex upstream). Off by default: on the platform, muster is the auth enforcement point in front of MCP servers. |
| oauth.baseURL | string | `""` | Public base URL of this server (https, or http on loopback only). |
| oauth.dex.issuerURL | string | `""` | Dex issuer URL. |
| oauth.dex.clientID | string | `""` | Dex OAuth client ID. |
| oauth.dex.clientSecret | string | `""` | Dex OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.existingSecret | string | `""` | Existing Secret with key `dex-client-secret`. |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"model-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Model management (inventory, pull, load/unload, delete, kagent wiring) for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| httpRoute.enabled | bool | `false` | Expose the service through a Gateway API HTTPRoute. |
| httpRoute.parentRefs | list | `[]` | parentRefs of the HTTPRoute (required when enabled). |
| httpRoute.hostnames | list | `[]` | Hostnames matched by the route. |
| httpRoute.annotations | object | `{}` | Annotations on the HTTPRoute. |
| httpRoute.labels | object | `{}` | Labels on the HTTPRoute. |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod. |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the API (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs (the host Ollama endpoint, e.g. 172.21.0.1/32). |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server (needed for wiring). |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the Role/RoleBinding for ModelConfigs and Secrets in `kagent.namespace` (and, when `backend` is kserve, the Role in the serving namespace, the Role for the discovery/preset ConfigMaps and the ClusterRole for nodes and PersistentVolumes). |
| kserve.namespace | string | `"model-serving"` | Serving namespace: InferenceServices, download Jobs, cache-tool pods and the cache claim live here and the kserve RBAC Role is created here. Must match the platform's `components.modelServing.namespace.name`. |
| kserve.discovery.configMap | string | `"agent-platform-model-serving"` | Name of the platform's model-serving discovery ConfigMap (kind `ModelServingConfig`, key `config.yaml`) that carries the runtime, GPU resource name, cache claim and preset selector. Empty `kserve.*` overrides below take their value from it. |
| kserve.discovery.namespace | string | `""` | Namespace of the discovery ConfigMap and, by default, of the preset ConfigMaps. Empty means the release namespace. |
| kserve.runtime | string | `""` | ClusterServingRuntime for presets that name none; empty takes the discovery value (default `kserve-vllm`). |
| kserve.gpuResourceName | string | `""` | Accelerator resource name; empty takes the discovery value (default `nvidia.com/gpu`). |
| kserve.cache.claimName | string | `""` | PersistentVolumeClaim of the Hugging Face cache in the serving namespace; empty takes the discovery value (default `hf-cache`). |
| kserve.cache.mountPath | string | `""` | Where predictors mount the cache; empty takes the discovery value (default `/mnt/models`). |
| kserve.cache.nodes | list | `[]` | Nodes that hold the cache. Empty derives them from the node affinity of the PersistentVolume bound to the claim (a static local PV pins the cache to its node). |
| kserve.presets.namespace | string | `""` | Namespace of the serving-preset ConfigMaps; empty takes the discovery value, else `kserve.discovery.namespace`. |
| kserve.presets.labelSelector | string | `""` | Label selector of the serving-preset ConfigMaps; empty takes the discovery value (default `agent-platform.giantswarm.io/serving-preset=true`). |
| kserve.hf.endpoint | string | `"https://huggingface.co"` | Hugging Face Hub base URL (search, repository metadata, sizes). |
| kserve.hf.tokenSecret.name | string | `""` | Secret in the serving namespace holding a Hugging Face token for gated repositories (read by model-manager, mounted into download Jobs). Empty: anonymous hub access, gated models refused. |
| kserve.hf.tokenSecret.key | string | `"token"` | Key of the token in that Secret. |
| kserve.hf.token | string | `""` | Render the token Secret (`kserve.hf.tokenSecret.name`, in the serving namespace) from this value. Prefer a pre-created Secret; this is for installs whose values are already secret-managed. |
| kserve.download.image | object | `{"name":"kserve/storage-initializer","registry":"docker.io","tag":"v0.20.0"}` | Image of the pre-warm download Job. The KServe storage-initializer downloads exactly what an InferenceService would, so a later start finds the files and skips the download. |
| kserve.download.ignorePatterns | list | `[]` | File patterns downloads skip (fnmatch, passed as STORAGE_IGNORE_PATTERNS). Empty downloads the whole repository like an InferenceService does. |
| kserve.download.jobTTL | string | `"1h"` | ttlSecondsAfterFinished of download Jobs. |
| kserve.inventory.image | object | `{"name":"giantswarm/alpine","registry":"gsoci.azurecr.io","tag":"3.22.1"}` | Image that creates cache directories (download Job init container) and scans / cleans the cache (short-lived pods); needs sh, find, stat, awk (busybox). |
| kserve.inventory.ttl | string | `"2m"` | How long one cache scan is reused before a node is scanned again. |
| kserve.inventory.timeout | string | `"2m"` | Time budget of one scan pod. |
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
