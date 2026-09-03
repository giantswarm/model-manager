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
| Backend identity, capabilities + load semantics | `GET /api/v1/backend` | `get_backend` |
| Downloaded models (with loaded state + ModelConfig) | `GET /api/v1/models`, `GET /api/v1/models/{name}` | `list_models`, `get_model` |
| Loaded / running models | `GET /api/v1/loaded` | `list_loaded_models` |
| Pull / import (returns a job) | `POST /api/v1/models/pull {"model","wire?","preset?","node?"}` | `pull_model` |
| Job progress | `GET /api/v1/jobs`, `GET /api/v1/jobs/{id}`, `DELETE /api/v1/jobs/{id}` | `list_jobs`, `get_job`, `cancel_job` |
| Load / unload | `POST /api/v1/models/load {"model","keepAlive?"}`, `POST /api/v1/models/unload` | `load_model`, `unload_model` |
| Delete (unwires by default) | `DELETE /api/v1/models/{name}[?unwire=false]` | `delete_model` |
| Wire / unwire to kagent | `POST /api/v1/models/wire`, `POST /api/v1/models/unwire` | `wire_model`, `unwire_model` |
| Serving presets (kserve) | `GET /api/v1/presets` | `list_presets` |
| Hub search (kserve) | `GET /api/v1/search?q=…&limit=…` | `search_models` |
| Fit check (kserve) | `POST /api/v1/models/fit-check {"model" or "preset","node?"}` | `check_fit` |
| Node budgets + reservations (+ caches on kserve) | `GET /api/v1/nodes` | `list_nodes` |
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
`nodeInventory`, `search` (`presets`, `fitCheck` and `search` are kserve
concerns and false on ollama).

`GET /api/v1/backend` reports two addresses: `endpoint`, the backend as
model-manager dials it, and `agentEndpoint`, the backend as **agent pods** dial
it — the host written into ModelConfigs (ollama: `--ollama-agent-host`,
defaulting to the endpoint). A client that matches ModelConfigs it did not
create to models by hostname (the portal's "Used by") compares against
`agentEndpoint`. kserve omits it: every served model has its own predictor URL
(`running.endpoint`, `modelConfig.endpoint`).

On ollama, `GET /api/v1/nodes` reports the proxied host as one node so a
laptop install has capacity data too. Ollama's API has no capacity endpoint,
so `budgetBytes` is `MemTotal` of `/proc/meminfo` as the model-manager pod
sees it (`budgetSource: host-meminfo`) — on a unified-memory machine GPU memory
is system memory, so that is the budget that matters; `reservedBytes` is the
sum of `size` over Ollama's `/api/ps` (weights plus KV cache for the loaded
context, which is why a 500 MiB download can reserve 5 GB), `accelerated` says
whether any loaded model has memory on an accelerator (`size_vram` > 0), and
each model's own share stays in `running.vramBytes`. There is no `gpuCount`,
`gpuProduct` or `cache` — Ollama does not expose the accelerator, and its model
store is not a node cache. Caveat: the pod reads the kernel it runs on. On kind
or any install sharing the machine's kernel that is the machine's RAM; under a
VM-backed container runtime (Docker Desktop, Podman machine) it is the VM's,
and for an Ollama on another machine it says nothing about that host. For those
installs the chart value `ollama.memoryBudgetGiB` (`--ollama-memory-budget-gib`,
`MODEL_MANAGER_OLLAMA_MEMORY_BUDGET_GIB`; GiB, decimals allowed) sets the budget
instead: `budgetBytes` is that figure with `budgetSource: override`, `message`
says so, and `allocatableMemoryBytes` stays the pod's `MemTotal` — the ollama
counterpart of the kserve node annotation
`model-manager.giantswarm.io/memory-budget-gib`. A value that is not a positive
number of GiB is ignored and named in `message`; the budget then comes from
`/proc/meminfo` as before.

## Load semantics: on-demand load and keep-alive

`GET /api/v1/backend` also carries a `loading` block so a client can word a
not-loaded model correctly without keying off the backend name:

```json
"loading": { "onDemand": true, "idleEviction": true, "keepAliveDefault": "5m", "keepAliveScope": "request" }
```

- **ollama** — `onDemand: true`: Ollama loads a model on the first `/api/chat`
  (or `/api/generate`) that names it, so an agent whose ModelConfig points at a
  not-loaded model works and its first turn pays the cold start. "Not loaded"
  is idle, not broken. `idleEviction: true`: Ollama evicts a model when its
  keep-alive runs out. `keepAliveScope: request`: the keep-alive is set **per
  request** — Ollama's scheduler takes each request's `keep_alive`, else the
  server's `OLLAMA_KEEP_ALIVE` (5m unless set), and re-arms the runner's timer
  on every hit. kagent's Ollama provider sends no `keep_alive`, so every agent
  turn re-arms the **server default**. `POST /api/v1/models/load` (a generate
  call with `keep_alive`, default `--default-keep-alive` /
  `MODEL_MANAGER_DEFAULT_KEEP_ALIVE`, reported as `keepAliveDefault`) therefore
  only **pre-warms**: it answers with the loaded model and `running.expiresAt`
  — the deadline Ollama reports right then — and the next agent request resets
  the timer to the server default again, even after a load with `-1`.
- **kserve** — `onDemand: false`, `idleEviction: false`, no keep-alive fields:
  a stopped InferenceService does not come back on request, agents on its
  ModelConfig fail at their first turn, and a running one stays until unloaded.

The knob that changes what agents experience on Ollama is host-side, not a
model-manager flag: set `OLLAMA_KEEP_ALIVE=30m` (or `-1` for never) in the
Ollama service environment — the systemd unit's `Environment=` on Linux —
and restart Ollama. `keepAliveDefault` is model-manager's default for its own
load requests; the host's `OLLAMA_KEEP_ALIVE` is not observable through the
API, which is why the block does not claim to report it.

## The kserve backend

The driver consumes the `modelServing` contract of `agent-platform-standalone`:
the discovery ConfigMap `agent-platform-model-serving` (kind
`ModelServingConfig`) for the serving namespace, runtime, GPU resource name,
cache claim and preset selector; the `ServingPreset` ConfigMaps
(`agent-platform.giantswarm.io/serving-preset=true`); the cache
PersistentVolumeClaim in the serving namespace. Every discovered value can be
overridden by a flag (`model-manager serve --help`, `--kserve-*`).

- **Inventory** — the cache contents per node plus the InferenceServices of
  the serving namespace (readiness from conditions/`modelStatus`, node from the
  predictor pod, GPU request, predictor URL). The cache is read in one of two
  ways (`kserve.inventory.mode`): a **short-lived scan pod** per cache node
  mounts the claim read-only and walks `<claim>/<dir>` whenever the inventory
  is older than the TTL (default), or a **DaemonSet** of `model-manager
  cache-agent` pods (same image) mounts the claim on each selected node and
  serves the same walk at `GET /inventory`, which model-manager reads from
  the agent on the node — no pod churn, and `nodes[].cache.inventory` says
  which mode produced the numbers. A node without a ready agent reports
  `cache.error`. The node holding the cache comes from the bound
  PersistentVolume's node affinity. `GET /api/v1/nodes` says which nodes hold
  a cache at all and what their memory budget is.
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

## Jobs, restarts and replicas

Jobs (`GET /api/v1/jobs`) live in process memory: the chart runs one replica
and clients poll one server. On kserve a pull job carries the `node` whose
cache receives the download and the `preset` it is for — what the request
named, or what model-manager picked itself after the fit check — so any
client (another tab, an agent through MCP) can place the download without
remembering the request; ollama jobs carry neither. A restart loses the job
list, not the work — kserve pulls are Kubernetes Jobs that model-manager
re-adopts on start (`GET /api/v1/jobs` lists them again as running pulls,
node and preset read back from the Job's annotations), a kserve `load` is
recovered by the reconcile loop that wires ready InferenceServices without a
job, and an ollama pull simply is re-issued (Ollama resumes the layers it has).
A persistent job store is deliberately not built until a second replica or a
job history across restarts is needed; until then, treat the job list as a
progress view, and the backend (Jobs, InferenceServices, Ollama) as the truth.

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
