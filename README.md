# model-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/model-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/model-manager/tree/main)

Model management service for the Giant Swarm Agent Platform. One API over
per-installation **serving backends**:

| Backend | Where | What it proxies |
|---|---|---|
| `ollama` | laptop / agentlab installs (host Ollama through the kind docker-network gateway) | `/api/tags`, `/api/ps`, streamed `/api/pull`, `/api/delete`, `keep_alive` load/unload |
| `kserve` | GPU installs (KServe + the platform's `modelServing` component) | InferenceServices composed from serving presets, per-node HF cache inventory, pre-warm download Jobs with progress, Hugging Face Hub search, node fit checks |

The API reports the backend and **explicit capability flags**
(`GET /api/v1/backend`), so clients render only what an installation supports
instead of switching on the backend name. Pulled or loaded models are wired
into kagent automatically as `ModelConfig`s (native keyless `Ollama` provider
for the ollama backend; `OpenAI` provider against the predictor URL plus a
placeholder API-key Secret for kserve, created once the InferenceService is
ready), so agents can use them without manual steps.

The same operations are exposed twice from one process:

- **REST/JSON** under `/api/v1` for the portal backend — contract in
  [`api/openapi.yaml`](api/openapi.yaml), also served at `/api/v1/openapi.yaml`.
- **MCP** (streamable HTTP, `/mcp`) for muster — tools `get_backend`,
  `list_models`, `get_model`, `list_loaded_models`, `pull_model`, `load_model`,
  `unload_model`, `delete_model`, `wire_model`, `unwire_model`, `list_jobs`,
  `get_job`, `cancel_job`, plus the kserve capabilities `list_presets`,
  `search_models`, `check_fit`, `list_nodes` (through muster: `x_<server>_<tool>`).

Part of [Model management in the Agent Platform](https://github.com/giantswarm/giantswarm/issues/37590);
decision records: Model Manager PDR and the Ollama-backend ADR in the team's
decision log.

## API at a glance

| Operation | REST | MCP tool |
|---|---|---|
| Backend identity + capabilities | `GET /api/v1/backend` | `get_backend` |
| Downloaded models (with loaded state + ModelConfig) | `GET /api/v1/models`, `GET /api/v1/models/{name}` | `list_models`, `get_model` |
| Loaded / running models | `GET /api/v1/loaded` | `list_loaded_models` |
| Pull / import (returns a job) | `POST /api/v1/models/pull {"model","wire?"}` | `pull_model` |
| Job progress | `GET /api/v1/jobs`, `GET /api/v1/jobs/{id}`, `DELETE /api/v1/jobs/{id}` | `list_jobs`, `get_job`, `cancel_job` |
| Load / unload | `POST /api/v1/models/load {"model","keepAlive?"}`, `POST /api/v1/models/unload` | `load_model`, `unload_model` |
| Delete (unwires by default) | `DELETE /api/v1/models/{name}[?unwire=false]` | `delete_model` |
| Wire / unwire to kagent | `POST /api/v1/models/wire`, `POST /api/v1/models/unwire` | `wire_model`, `unwire_model` |
| Serving presets (kserve) | `GET /api/v1/presets` | `list_presets` |
| Hub search (kserve) | `GET /api/v1/search?q=…&limit=…` | `search_models` |
| Fit check (kserve) | `POST /api/v1/models/fit-check {"model" or "preset","node?"}` | `check_fit` |
| Node budgets + caches (kserve) | `GET /api/v1/nodes` | `list_nodes` |
| Health | `GET /healthz`, `GET /readyz` | — |

Model references may contain `/` and `:` (`smollm2:135m`, `hf.co/org/repo:Q4_K_M`,
`Qwen/Qwen3-14B`); path parameters capture the rest of the path. Errors are
`{"error":{"code":"not_found|invalid_request|unsupported|conflict|does_not_fit|backend_error","message":"…"}}`;
`unsupported` (501) means the matching capability flag is false, `does_not_fit`
(412) that the kserve fit check refused a pull or load.

On kserve, `pull` and `load` accept `preset` and `node`; a model is wired into
kagent when its InferenceService becomes ready (a `load` job tracks that) and
unwired on unload — never after a pull, since a cached model has no endpoint.

Capability flags: `pull`, `pullProgress`, `delete`, `load`, `unload`,
`loadedModels`, `wire` (Kubernetes access present), `presets`, `fitCheck`,
`nodeInventory`, `search` (the last four are kserve concerns and false on ollama).

`GET /api/v1/backend` reports two addresses: `endpoint`, the backend as
model-manager dials it, and `agentEndpoint`, the backend as **agent pods** dial
it — the host written into ModelConfigs (ollama: `--ollama-agent-host`,
defaulting to the endpoint). A client that matches ModelConfigs it did not
create to models by hostname (the portal's "Used by") compares against
`agentEndpoint`. kserve omits it: every served model has its own predictor URL
(`running.endpoint`, `modelConfig.endpoint`).

## The kserve backend

The driver consumes the `modelServing` contract of `agent-platform-standalone`:
the discovery ConfigMap `agent-platform-model-serving` (kind
`ModelServingConfig`) for the serving namespace, runtime, GPU resource name,
cache claim and preset selector; the `ServingPreset` ConfigMaps
(`agent-platform.giantswarm.io/serving-preset=true`); the cache
PersistentVolumeClaim in the serving namespace. Every discovered value can be
overridden by a flag (`model-manager serve --help`, `--kserve-*`).

- **Inventory** — the cache contents per node (a short-lived pod mounts the
  claim read-only and walks `<claim>/<dir>`; the node comes from the bound
  PersistentVolume's node affinity) plus the InferenceServices of the serving
  namespace (readiness from conditions/`modelStatus`, node from the predictor
  pod, GPU request, predictor URL). `GET /api/v1/nodes` says which nodes hold a
  cache at all and what their memory budget is.
- **Import** — `search` proxies the Hugging Face Hub; `fit-check` resolves the
  weight size (`model.safetensors.index.json`, else the file tree, else the
  preset), adds the preset's `overheadGiB` (default 30) and compares with the
  node budget (`nvidia.com/gpu.memory` x `gpu.count` labels when present, else
  allocatable memory — unified-memory nodes; a node annotation
  `model-manager.giantswarm.io/memory-budget-gib: "96"` overrides that node's
  budget in GiB whatever `kserve.budget.source` says, reported as
  `budgetSource: annotation` — for unified-memory nodes whose allocatable
  memory overstates what a model may use); `pull` refuses what cannot be
  served, then runs a download Job with the KServe storage-initializer image
  into `<claim>/<preset name>` — the directory the preset's InferenceService
  mounts — reporting bytes on disk against the repository size. Gated models
  need a token Secret (`--kserve-hf-token-secret`).
- **Serve / stop** — `load` composes an InferenceService from the preset
  (runtime, format, storageUri, args, env, chat-template mount, GPU count;
  nodeSelector, runtimeClassName, deployment strategy and timeout from
  discovery; `spec.predictor` extras verbatim) after a fit check against the
  node's free budget; `unload` deletes it (the cache persists); `delete`
  removes the cache directory (refused while served).
- **Wiring** — on ready, a kagent `ModelConfig` **named after the
  InferenceService** (`provider: OpenAI`, `baseUrl` = predictor URL + `/v1`,
  `model` = InferenceService name, which vLLM serves under
  `--served-model-name {{.Name}}`) plus the placeholder `OPENAI_API_KEY`
  Secret the go ADK runtime insists on — the same rule the portal's serve
  flow applies. A ModelConfig that already points at the predictor (same
  host, same served model name), whoever created it, counts as the model's
  wiring: it is reported with `managed: false`, never duplicated and never
  deleted; `unwire`/`unload` only remove ModelConfigs model-manager created.
- **Ownership** — InferenceServices in the serving namespace that
  model-manager created or that carry the `agent-platform.giantswarm.io/preset`
  label (the portal's serve flow) can be unloaded here; hand-written ones are
  inventory only (`409 conflict` on unload; `managedBy` says who owns them).

## Running

```sh
model-manager serve \
  --backend ollama \
  --ollama-endpoint http://127.0.0.1:11434 \          # as reached by model-manager
  --ollama-agent-host http://172.21.0.1:11434 \        # as reached by agent pods (written into ModelConfigs)
  --kubeconfig ~/.kube/config --kube-context kind-agentlab \
  --kagent-namespace kagent
```

Every flag has an environment variable (`model-manager serve --help`). Without
Kubernetes access the server still runs; `wire` reports `false` and wiring
operations answer 501. OAuth in front of `/mcp` is available (`--enable-oauth`,
Dex) but off by default: on the platform muster is the auth enforcement point.

## Helm chart

`helm/model-manager` — see its [README](helm/model-manager/README.md). Keys the
umbrella chart (`agent-platform-standalone`) sets: `backend`, `ollama.endpoint`,
`ollama.agentHost`, `kagent.namespace`, `image.*`, `mcp.enabled`,
`muster.mcpServer.*`; for kserve `kserve.namespace` (the serving namespace),
`kserve.discovery.*`, `kserve.hf.tokenSecret.*` and the `kserve.*` overrides. Optional, off by default: `muster.mcpServer.enabled`
(renders an `mcpservers.muster.giantswarm.io` CR), `httpRoute.enabled`,
`networkPolicy.enabled`, `oauth.enabled`.

## Development

See [docs/development.md](docs/development.md).
