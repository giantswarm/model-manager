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
- `kserve` — InferenceServices, per-node HF cache, download Jobs, presets
  (driver lands in a later release; the RBAC block is already templated).

Used as a dependency of `agent-platform-standalone` behind a `condition`;
standalone installs set `ollama.endpoint`, `kagent.namespace`, `image.*`,
`mcp.enabled` and `muster.mcpServer.*`.

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
| backend | string | `"ollama"` | Serving backend driver: `ollama` (host Ollama — laptop/agentlab dev loop) or `kserve` (KServe/vLLM on GPU installs; driver lands in a later release). The API reports the backend and its capability flags at /api/v1/backend. |
| ollama.endpoint | string | `"http://host.docker.internal:11434"` | Ollama API base URL as reached from pods. On kind this is the docker network gateway (for example http://172.21.0.1:11434 — agentlab sets it); Docker Desktop resolves host.docker.internal. |
| ollama.agentHost | string | `""` | Ollama host written into kagent ModelConfigs, as reached by agent pods. Empty means the same as `ollama.endpoint`. |
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
| rbac.create | bool | `true` | Create the Role/RoleBinding for ModelConfigs and Secrets in `kagent.namespace` (and the KServe rules when `backend` is kserve). |
| kserve.namespace | string | `""` | Namespace holding InferenceServices and download Jobs (kserve backend). |
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
