# model-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/model-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/model-manager/tree/main)

Model management service for the Giant Swarm Agent Platform. One API over
**serving backends** — one or several per installation, in one process:

| Backend | Where | What it proxies |
|---|---|---|
| `ollama` | laptop / agentlab installs (host Ollama through the kind docker-network gateway) | `/api/tags`, `/api/ps`, streamed `/api/pull`, `/api/delete`, `keep_alive` load/unload |
| `kserve` | GPU installs (KServe + the platform's `modelServing` component) | InferenceServices composed from serving presets, per-node HF cache inventory, pre-warm download Jobs with progress, Hugging Face Hub search, node fit checks |
| `lemonade` | AMD Ryzen AI laptops / workstations running [Lemonade Server](https://lemonade-server.ai) on the host (FastFlowLM on the NPU, llama.cpp on GPU / CPU) | `/api/v1/health` (loaded models), `/api/v1/models`, streamed `/api/v1/pull`, `/api/v1/load`, `/api/v1/unload`, `/api/v1/delete`, `/api/v1/system-info` |

One model-manager runs every backend the host has (`--backends=ollama,lemonade`,
chart value `backends: [ollama, lemonade]`): the API reports each backend and
its **explicit capability flags** (`GET /api/v1/backends`), every model, job and
node names its `backend`, every request may name one, and an unqualified
model reference is resolved to the one backend that holds it (`409 conflict`
when several do). `GET /api/v1/backend` describes the first, default backend
and names the others — the one-backend form clients of a single backend keep
using. Clients render only what a backend supports instead of switching on
its name. Pulled or loaded models are wired
into kagent automatically as `ModelConfig`s (native keyless `Ollama` provider
for the ollama backend; `OpenAI` provider plus a placeholder API-key Secret —
against the predictor URL for kserve, created once the InferenceService is
ready, and against Lemonade's `/api/v1` for lemonade), so agents can use them
without manual steps.

The same operations are exposed twice from one process:

- **REST/JSON** under `/api/v1` for the portal backend — contract in
  [`api/openapi.yaml`](api/openapi.yaml), also served at `/api/v1/openapi.yaml`.
- **MCP** (streamable HTTP, `/mcp`) for muster — tools `get_backend`,
  `list_backends`, `list_models`, `get_model`, `list_loaded_models`, `pull_model`,
  `load_model`, `unload_model`, `delete_model`, `wire_model`, `unwire_model`,
  `list_jobs`, `get_job`, `cancel_job`, plus the kserve capabilities
  `list_presets`, `search_models`, `check_fit`, `list_nodes` (through muster:
  `x_<server>_<tool>`); every tool takes an optional `backend`.

Part of [Model management in the Agent Platform](https://github.com/giantswarm/giantswarm/issues/37590);
decision records: Model Manager PDR, the Ollama-backend ADR and the
Lemonade-backend ADR in the team's decision log.

## API at a glance

| Operation | REST | MCP tool |
|---|---|---|
| Every backend's identity, capabilities + load semantics (the first is the default) | `GET /api/v1/backends` | `list_backends` |
| One backend (the named one, else the default) plus the names of all | `GET /api/v1/backend[?backend=]` | `get_backend` |
| Downloaded models (with loaded state + ModelConfig), of one or every backend | `GET /api/v1/models[?backend=]`, `GET /api/v1/models/{name}[?backend=]` | `list_models`, `get_model` |
| Loaded / running models | `GET /api/v1/loaded[?backend=]` | `list_loaded_models` |
| Pull / import (returns a job; on `backend`, else the default backend) | `POST /api/v1/models/pull {"model","backend?","wire?","preset?","node?"}` | `pull_model` |
| Job progress | `GET /api/v1/jobs[?backend=]`, `GET /api/v1/jobs/{id}`, `DELETE /api/v1/jobs/{id}` | `list_jobs`, `get_job`, `cancel_job` |
| Load / unload | `POST /api/v1/models/load {"model","backend?","keepAlive?"}`, `POST /api/v1/models/unload {"model","backend?"}` | `load_model`, `unload_model` |
| Delete (unwires by default) | `DELETE /api/v1/models/{name}[?unwire=false][&backend=]` | `delete_model` |
| Wire / unwire to kagent | `POST /api/v1/models/wire {"model","backend?"}`, `POST /api/v1/models/unwire {"model","backend?"}` | `wire_model`, `unwire_model` |
| Serving presets (kserve) | `GET /api/v1/presets[?backend=]` | `list_presets` |
| Hub search (kserve) | `GET /api/v1/search?q=…&limit=…[&backend=]` | `search_models` |
| Fit check (kserve) | `POST /api/v1/models/fit-check {"model" or "preset","backend?","node?"}` | `check_fit` |
| Node budgets + reservations (+ caches on kserve) | `GET /api/v1/nodes[?backend=]` | `list_nodes` |
| Health | `GET /healthz`, `GET /readyz` | — |

Model references may contain `/` and `:` (`smollm2:135m`, `hf.co/org/repo:Q4_K_M`,
`Qwen/Qwen3-14B`); path parameters capture the rest of the path. Errors are
`{"error":{"code":"not_found|invalid_request|unsupported|conflict|does_not_fit|backend_error","message":"…"}}`;
`unsupported` (501) means the matching capability flag is false, `does_not_fit`
(412) that the kserve fit check refused a pull or load, `conflict` (409) also
that an unqualified reference exists on several backends — repeat the request
with `backend`.

**Several backends in one process.** Every `Model`, `LoadedModel`, `Job`,
`NodeInfo`, `Preset`, `FitResult` and `ModelConfigRef` carries `backend`. Reads
without `?backend=` aggregate every backend; a backend that fails to answer is
reported in the response's `errors` (`{"lemonade": "connection refused"}`)
while the others' items are returned — `502` only when every backend failed.
An unknown or unconfigured backend name answers `400`. kagent ModelConfigs are
one per (backend, model): the label `model-manager.giantswarm.io/backend` plus
the model annotation identify them, and the same reference on two backends is
two ModelConfigs (the second named `<derived>-<backend>`).

On kserve, `pull` and `load` accept `preset` and `node`; a model is wired into
kagent when its InferenceService becomes ready (a `load` job tracks that) and
unwired on unload — never after a pull, since a cached model has no endpoint.

Capability flags: `pull`, `pullProgress`, `delete`, `load`, `unload`,
`loadedModels`, `wire` (Kubernetes access present), `presets`, `fitCheck`,
`nodeInventory`, `search` (`presets`, `fitCheck` and `search` are kserve
concerns and false on ollama and lemonade).

`GET /api/v1/backend` reports two addresses: `endpoint`, the backend as
model-manager dials it, and `agentEndpoint`, the backend as **agent pods** dial
it — the host written into ModelConfigs (ollama: `--ollama-agent-host`,
defaulting to the endpoint; lemonade: `--lemonade-agent-host` plus `/api/v1`,
the OpenAI-compatible base URL the ModelConfigs carry). A client that matches
ModelConfigs it did not
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

On lemonade the host node comes from Lemonade's own `GET /api/v1/system-info`
instead (`budgetSource: system-info`, the accelerators Lemonade enumerates as
`gpuCount` / `gpuProduct`, the model store as `cache`) — see
[The lemonade backend](#the-lemonade-backend).

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
- **lemonade** — `onDemand: true`: Lemonade loads a model on the first
  completion naming it (a few seconds for a 4B model on the NPU), so a
  not-loaded model is idle, not broken. `idleEviction: false`, no keep-alive
  fields: nothing evicts an idle model, but a loaded model gives way when
  another model of its type is requested and the slot is taken
  (`max_loaded_models`, one per type by default; least recently used first)
  unless it was loaded with `keepAlive: "-1"`, which pins it.

The knob that changes what agents experience on Ollama is host-side, not a
model-manager flag: set `OLLAMA_KEEP_ALIVE=30m` (or `-1` for never) in the
Ollama service environment — the systemd unit's `Environment=` on Linux —
and restart Ollama. `keepAliveDefault` is model-manager's default for its own
load requests; the host's `OLLAMA_KEEP_ALIVE` is not observable through the
API, which is why the block does not claim to report it.

## The lemonade backend

[Lemonade Server](https://lemonade-server.ai) is AMD's local model server: one
OpenAI-compatible API (`/api/v1`) in front of several runtimes — "recipes" —
of which FastFlowLM (`flm`) runs models on the Ryzen AI NPU and llama.cpp
(`llamacpp`) on the GPU (Vulkan, ROCm) or CPU. Like Ollama it ships its
management surface on the same port, and the driver has the same shape as the
ollama one: a proxy of `GET /api/v1/health` (the loaded models),
`GET /api/v1/models`, the streamed `POST /api/v1/pull`, `POST /api/v1/load`,
`POST /api/v1/unload`, `POST /api/v1/delete` and `GET /api/v1/system-info`.
Lemonade stays on the host — that is where the NPU driver is — and pods reach
it through the docker network gateway, so bind it to every interface
(`lemonade config set host=0.0.0.0`; its port is 13305 by default) and let
the bridge subnets through the host firewall, as for Ollama.

- **Inventory** — `GET /api/v1/models` lists the downloaded models: the
  Lemonade id is the `name` (`qwen3-it-4b-FLM`, `Qwen3-0.6B-GGUF`), `size` in
  GB becomes `sizeBytes`, the recipe is reported as `runtime` (`flm`,
  `llamacpp`, ...), the format and quantization are read off the checkpoint
  (`unsloth/Qwen3-0.6B-GGUF:Q4_0` → `gguf`, `Q4_0`) and Lemonade's labels are
  mapped onto the capability vocabulary the other backends use (`chat` →
  `completion`, `tool-calling` → `tools`, `reasoning` → `thinking`,
  `embeddings` → `embedding`, `vision` stays; a text recipe without a
  deployment label is a `completion` model). Everything Lemonade has
  downloaded is listed — transcription, image or speech models included —
  and the capabilities say what each one is.
- **Pull** — `POST /api/v1/models/pull` takes a Lemonade catalog model name
  (`lemonade list`, or `GET /api/v1/models?show_all=true` on the server) and
  streams Lemonade's install (`stream: true`): the job reports bytes done
  against the whole download when Lemonade says how big it is, else against
  the current file. A name that is not in the catalog fails with `not_found`
  (registering a `user.*` model from a Hugging Face checkpoint is not offered
  here). On success the model is wired into kagent, as on ollama.
- **Load / unload** — `POST /api/v1/load` starts the model's backend process
  (FastFlowLM on the NPU, llama.cpp, ...) and answers once it is ready; a
  load is bounded by ten minutes, not the usual call timeout. Lemonade has no
  keep-alive: a loaded model stays until it is unloaded or until another
  model of its type needs the slot (`max_loaded_models`, one per type by
  default; least recently used first). `keepAlive: "-1"` pins the model
  against that displacement (Lemonade's `pinned`, reported as
  `running.pinned`); every other keep-alive is ignored. `running.device` says
  where the model runs (`npu`, `gpu`, `cpu`). Unloading a model that is not
  loaded is a no-op.
- **Wiring** — a kagent `ModelConfig` named after the model
  (`qwen3-it-4b-FLM` → `qwen3-it-4b-flm`) with `provider: OpenAI`,
  `openAI.baseUrl` = the agent host plus `/api/v1` (`--lemonade-agent-host`,
  defaulting to the endpoint; reported as `agentEndpoint`) and the
  placeholder `OPENAI_API_KEY` Secret the kagent runtime insists on — the
  same path the kserve backend takes to a vLLM predictor.
- **Node** — `GET /api/v1/nodes` reports the host as Lemonade sees it: memory
  from `Physical Memory` of `GET /api/v1/system-info` (`budgetSource:
  system-info`), the accelerators Lemonade found available as `gpuCount` /
  `gpuProduct` (the NPU when there is one — `AMD NPU (NPU Strix)` — else the
  first GPU), reservations as the catalog sizes of the loaded models (Lemonade
  reports no per-model memory), and Lemonade's model store as the node's
  `cache` (`mountPath`, models, bytes).

Nothing here needs Kubernetes access beyond wiring, exactly as for ollama.

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
- **Naming a cache directory** — a directory is named by what is known to
  have filled it: the marker a pre-warm download Job wrote, else the **cache
  index** — a ConfigMap in the serving namespace (`model-manager-cache-index`,
  `kserve.cache.indexConfigMap`) in which the driver records, while an
  InferenceService exists, that its name is the directory the KServe
  storage-initializer fills and its `hf://` storageUri the repository
  (repository, revision, preset label, one JSON entry per directory). The
  record outlives the InferenceService, so the directory keeps its repository
  and, through it, its preset (the labelled one, else the single preset
  serving the repository) after the InferenceService is deleted; a live
  InferenceService always wins over a stale record, and the record is dropped
  when model-manager removes the directory. Without a marker or record the
  preset or InferenceService of the same name is assumed, else the directory
  is listed by its bare name. Directories whose top level holds no model —
  no `config.json` and no weights file (`*.safetensors`, `*.gguf`, `*.bin`,
  `*.pt`, `*.pth`, `*.onnx`) — are not listed as downloads: Hugging Face
  client internals such as `hf-home` and `xet` live on the same claim and
  count towards `nodes[].cache.bytesUsed`, but they are not models, and a
  directory an InferenceService is still filling shows as that served model
  with `downloaded: false` until its files arrive.
- **Nodes and eligibility** — `GET /api/v1/nodes` lists the **accelerator
  nodes only**: nodes that advertise the configured GPU resource
  (`gpuResourceName` from discovery, default `nvidia.com/gpu`; capacity or
  allocatable > 0) or carry a gpu-feature-discovery label
  (`nvidia.com/gpu.present`, `.count`, `.product`). CPU-only nodes are not
  serving capacity for this backend. Each node says whether a model can be
  served there right now: `eligible` is true when the node is ready, matches
  the discovery `nodeSelector`, and can mount the cache claim whenever
  predictors mount it (cache enabled and `cache.redirectPolicy` on — the
  Kyverno rule that mounts the claim into every predictor) and the claim is
  pinned to nodes (a static local PersistentVolume or a `local-path` volume);
  a shared (RWX), unbound, missing or disabled cache never disqualifies a
  node. `eligibilityReason` names every failing rule (`not ready`, `outside
  the serving node selector (kubernetes.io/hostname=spark-8723)`, `cache claim
  hf-cache is pinned to spark-8723`). `pull`, `load` and `fit-check` refuse
  an explicit `node` that is not eligible with that reason (`412
  does_not_fit`; `fits=false` on the fit check) before any Job or
  InferenceService exists, and never pick an ineligible node themselves. A
  second node-local GPU node becomes a serving target only with per-node
  claims or shared storage — a chart decision, not a flag here.
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
job, and an ollama or lemonade pull simply is re-issued (Ollama resumes the layers
it has, Lemonade the files).
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

Against a Lemonade Server on the host (AMD Ryzen AI NPU):

```sh
model-manager serve \
  --backend lemonade \
  --lemonade-endpoint http://127.0.0.1:13305 \      # as reached by model-manager
  --lemonade-agent-host http://172.21.0.1:13305 \   # as reached by agent pods (+ /api/v1 in ModelConfigs)
  --kubeconfig ~/.kube/config --kube-context kind-agentlab
```

Every flag has an environment variable (`model-manager serve --help`). Without
Kubernetes access the server still runs; `wire` reports `false` and wiring
operations answer 501.

## Identity

With `--enable-oauth` model-manager is an OAuth 2.1 resource server
([mcp-oauth](https://github.com/giantswarm/mcp-oauth)) in front of both the
MCP endpoint and the REST API: every call needs a bearer token the platform
identity provider issued — Dex (`--oauth-provider dex`, the Agent Platform
default) or Google (`--oauth-provider google`). On the platform nobody logs in
to model-manager itself: muster forwards the session's IdP id_token
byte-identical (MCPServer `auth.forwardToken: true`, rendered by the chart)
and the portal sends the signed-in user's id_token through the gateway;
model-manager validates them against the IdP's JWKS when their audience is one
of `--oauth-trusted-audiences`. The chart passes the platform's OAuth client
(`oauth.trustedAudiences`, else `global.identity.clientId`) plus the
MCPServer's `requiredAudiences`: every forwarded token carries those by
construction and they are what the kube-apiserver trusts, so a portal session
— whose id_token carries them but not the platform client — is accepted too.
An id_token whose audience matches none of them is refused with 401, and the
refusal names the token's `aud` next to the trusted audiences (the log line
and the `WWW-Authenticate` `error_description`). The caller — the `email`,
else the `sub` — is on every mutation's log line and recorded as
`requestedBy` on jobs.

`--downstream-oauth` goes one step further: everything a request does against
the Kubernetes API (InferenceServices, download Jobs, cache scans, kagent
ModelConfigs, the discovery ConfigMap) presents the caller's IdP token, and
the ServiceAccount holds no permissions at all — the chart renders none of its
Roles and ClusterRoles; the caller's RBAC is the only RBAC. That needs an
apiserver that trusts the IdP and the token's audience — a Dex install lists
the cross-client audience the apiserver trusts in the MCPServer's
`requiredAudiences` (muster requests it at login), a Google install's client
id is the apiserver's `--oidc-client-id`. A request whose bearer yields no
IdP token to present (one this server neither issued nor trusts as a forwarded
id_token) is refused with 401 instead of running as the permissionless
ServiceAccount. Nothing runs without a caller: the
wiring reconciler and the re-adoption of running downloads after a restart
are off, and a job that outlives its caller's token (a download longer than
the token lives) fails on the apiserver's 401 — attributed to the caller,
retried by the caller. Health endpoints (`/healthz`, `/readyz`, `/backendz`)
and the OAuth metadata stay public.

## Helm chart

`helm/model-manager` — see its [README](helm/model-manager/README.md). Keys the
umbrella chart (`agent-platform-standalone`) sets: `backend`, `ollama.endpoint`,
`ollama.agentHost`, `lemonade.endpoint`, `lemonade.agentHost`,
`kagent.namespace`, `image.*`, `mcp.enabled`,
`muster.mcpServer.*`; for kserve `kserve.namespace` (the serving namespace),
`kserve.discovery.*`, `kserve.hf.tokenSecret.*` and the `kserve.*` overrides. Optional, off by default: `muster.mcpServer.enabled`
(renders an `mcpservers.muster.giantswarm.io` CR), `httpRoute.enabled`,
`networkPolicy.enabled`, `oauth.enabled`.

## Development

See [docs/development.md](docs/development.md).
