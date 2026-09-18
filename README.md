# model-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/model-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/model-manager/tree/main)

Model management service for the Giant Swarm Agent Platform. One API over
**serving backends** — one or several per installation, in one process:

| Backend | Where | What it proxies |
|---|---|---|
| `ollama` | laptop / agentlab installs (host Ollama through the kind docker-network gateway) | `/api/tags`, `/api/ps`, streamed `/api/pull`, `/api/delete`, `keep_alive` load/unload |
| `kserve` | GPU installs (KServe + the platform's `modelServing` component) | LLMInferenceServices (or classic InferenceServices) composed from serving presets, per-node HF cache inventory, pre-warm download Jobs with progress, Hugging Face Hub search, node fit checks |
| `lemonade` | AMD Ryzen AI laptops / workstations running [Lemonade Server](https://lemonade-server.ai) on the host (FastFlowLM on the NPU, llama.cpp on GPU / CPU) | `/api/v1/health` (loaded models), `/api/v1/models`, streamed `/api/v1/pull`, `/api/v1/load`, `/api/v1/unload`, `/api/v1/delete`, `/api/v1/system-info` |
| `lmstudio` | desktop installs running [LM Studio](https://lmstudio.ai) on the host (llama.cpp on GPU / CPU, MLX on Apple silicon) | `/api/v1/models` (the library and its loaded instances), `/api/v1/models/download` + its status job, `/api/v1/models/load`, `/api/v1/models/unload` — **no delete** |

One model-manager runs every backend the host has (`--backends=ollama,lmstudio`,
chart value `backends: [ollama, lmstudio]`): the API reports each backend and
its **explicit capability flags** (`GET /api/v1/backends`), every model, job and
node names its `backend`, every request may name one, and an unqualified
model reference is resolved to the one backend that holds it (`409 conflict`
when several do). `GET /api/v1/backend` describes the first, default backend
and names the others — the one-backend form clients of a single backend keep
using. Clients render only what a backend supports instead of switching on
its name. Pulled or loaded models are wired
into kagent automatically as `ModelConfig`s (native keyless `Ollama` provider
for the ollama backend; `OpenAI` provider with the caller's token forwarded
(`apiKeyPassthrough`) for a kserve model routed on the models Gateway;
`OpenAI` provider plus a placeholder API-key Secret —
against the predictor URL for a kserve model on its in-cluster Service, and
against Lemonade's `/api/v1` for lemonade; a kserve model's ModelConfig is
created by the load call, before the model is ready), so agents can use them
without manual steps. They are written in the kagent.dev API version the
cluster serves (`v1alpha3` on kagent API v2, discovered at start-up), and a
ModelConfig is `ready` once kagent has accepted it and resolved its Secret
(conditions `Accepted` and `ResolvedRefs`).

The same operations are exposed twice from one process:

- **REST/JSON** under `/api/v1` for the portal backend — contract in
  [`api/openapi.yaml`](api/openapi.yaml), also served at `/api/v1/openapi.yaml`.
- **MCP** (streamable HTTP, `/mcp`) for muster — tools `get_info`, `get_backend`,
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
| This server's build (release version, commit, build time), its tools, the configured backends and the wiring target | — | `get_info` |
| Every backend's identity, capabilities + load semantics (the first is the default) | `GET /api/v1/backends` | `list_backends` |
| Register / remove a backend at runtime (a backend document, [docs/backends.md](docs/backends.md); `dryRun`, `mode: apply`) | — | `add_backend`, `remove_backend` |
| One backend (the named one, else the default) plus the names of all | `GET /api/v1/backend[?backend=]` | `get_backend` |
| Downloaded models (with loaded state + ModelConfig), of one or every backend | `GET /api/v1/models[?backend=]`, `GET /api/v1/models/{name}[?backend=]` | `list_models`, `get_model` |
| Loaded / running models (kserve: each with its `modelConfig`; a served model model-manager manages that has none is wired by the read, `wiring: {wired, reason: "wired on read", modelConfig}`) | `GET /api/v1/loaded[?backend=]` | `list_loaded_models` |
| Pull / import (returns a job; on `backend`, else the default backend) | `POST /api/v1/models/pull {"model","backend?","wire?","preset?","node?"}` | `pull_model` |
| Job progress | `GET /api/v1/jobs[?backend=]`, `GET /api/v1/jobs/{id}`, `DELETE /api/v1/jobs/{id}` | `list_jobs`, `get_job`, `cancel_job` |
| Load / unload (kserve: the load answers `fit`, `running` and `wiring` — the ModelConfig created in the same call, `apiKeyPassthrough` for a model routed on the models Gateway — before the model is ready; the unload deletes the serving object and unwires within the call, never waiting for a cache scan, and answers `inventory: {refreshing, reason?}` — the cache rescanned in the background, or why it cannot be) | `POST /api/v1/models/load {"model","backend?","keepAlive?"}`, `POST /api/v1/models/unload {"model","backend?"}` | `load_model`, `unload_model` |
| Delete (unwires by default) | `DELETE /api/v1/models/{name}[?unwire=false][&backend=]` | `delete_model` |
| Wire / unwire to kagent (`apiKeyPassthrough` or `apiKeySecret`+`apiKeySecretKey` override the backend's API-key shape; both together are refused) | `POST /api/v1/models/wire {"model","backend?","apiKeyPassthrough?","apiKeySecret?","apiKeySecretKey?"}`, `POST /api/v1/models/unwire {"model","backend?"}` | `wire_model`, `unwire_model` |
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
kagent by the `load` call itself — before it is ready, at the address it will
answer on; the answer's `wiring` names the ModelConfig — and unwired on unload,
never after a pull, since a cached model has no endpoint. A `load` job follows
the model to readiness and refreshes the ModelConfig from the address KServe
published; the job lives in the process, so a restart loses only its progress
entry. A served model model-manager manages that has no ModelConfig is wired
by the caller's next `list_loaded_models` / `GET /api/v1/loaded`, whose entry
says so (`wiring.reason: "wired on read"`).

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
- **lmstudio** — `onDemand: true`: with LM Studio's just-in-time loading on
  (its default) the first completion naming a downloaded model loads it, so a
  not-loaded model is idle, not broken. `idleEviction: false`, no keep-alive
  fields: `POST /api/v1/models/load` is an **explicit** load, which LM Studio
  treats as manual — no idle TTL, and Auto-Evict leaves "non-JIT loaded
  models" alone — so a model loaded through model-manager stays resident
  until something unloads it. The endpoint accepts no `ttl`, which is why a
  keep-alive has nothing to map onto. **The two paths differ, and it matters
  for agents**: a model LM Studio JIT-loaded (what an agent turn does to a
  not-loaded model) *does* get its default idle TTL — 60 minutes — and *is*
  subject to Auto-Evict, so it can be gone later, while one loaded through
  this backend will not be. `loading` describes the backend's own loads.
  Turning
  just-in-time loading off in LM Studio is what breaks an agent on a
  not-loaded model — the request then fails instead of waiting.

The knob that changes what agents experience on Ollama is host-side, not a
model-manager flag: set `OLLAMA_KEEP_ALIVE=30m` (or `-1` for never) in the
Ollama service environment — the systemd unit's `Environment=` on Linux —
and restart Ollama. `keepAliveDefault` is model-manager's default for its own
load requests; the host's `OLLAMA_KEEP_ALIVE` is not observable through the
API, which is why the block does not claim to report it.

## Backends registered at runtime

model-manager starts with **no backend** (chart default `backend: ""`, `backends: []`) and gets its
backends at runtime: `add_backend` writes a **backend document** — a ConfigMap in model-manager's
namespace labelled `agent-platform.giantswarm.io/model-backend=true`, named `model-backend-<kind>`,
key `backend.yaml`, `kind: ModelBackend` — as the caller; model-manager watches the label, enforces
the document's schema when it reads it (an invalid document is reported under `invalid` in
`list_backends` with the failing field, and not loaded) and registers the backend without a
restart. `remove_backend` drops the backend's ModelConfigs and the document. One backend per kind;
`list_backends` reports every backend with `source: static | person | cluster-manager`, and a
`kserve` document carries the **target cluster** (`{cluster, organization, apiServer, caBundle,
servingNamespace}` — never credentials) the backend acts on, reported as `target` on the backend,
its models, nodes and presets. Static `--backends` values keep working as `source: static`; a
document naming a static kind is refused. With no backend at all every backend-scoped call answers
`no_backend` with the fix. The contract — the label, keys, schema, tools, RBAC and what
cluster-manager writes — is [docs/backends.md](docs/backends.md).

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

## The lmstudio backend

[LM Studio](https://lmstudio.ai) is a desktop model runner — llama.cpp on GPU
and CPU, MLX on Apple silicon — with its own API (`/api/v1`, 0.4.0 and newer)
next to an OpenAI-compatible one (`/v1`). The driver proxies the library
(`GET /api/v1/models`), the download that backs a pull
(`POST /api/v1/models/download` plus
`GET /api/v1/models/download/status/{job_id}`) and
`POST /api/v1/models/load` / `POST /api/v1/models/unload`. Like Ollama and
Lemonade it stays on the host, so serve it on the local network
(`lms server start --bind 0.0.0.0`, the app's Developer → "Serve on Local
Network" toggle, or `LMS_SERVER_HOST=0.0.0.0`; its port is 1234 by default)
and let the bridge subnets through the host firewall.

Two traits set it apart from the other host backends:

- **It has no delete.** Removing a model is `lms rm` on the host, which a pod
  cannot run, so the driver reports `delete: false` and the API answers
  `501 unsupported`. Use `POST /api/v1/models/unwire` to drop just the
  ModelConfig; the weights stay on the host until someone removes them there.
- **It has no version or health endpoint, and it answers HTTP 200 with an
  `{"error": …}` document for every path outside `/api/v1`** — Ollama's
  `/api/version`, `/api/tags` and `/api/show` included. Nothing may read a
  status code as proof that it reached an LM Studio: health here means
  `GET /api/v1/models` answering with a `models` array, and `version` stays
  empty because the server reports none.

- **Inventory** — `GET /api/v1/models` is the local library, so every entry is
  downloaded: the `key` is the name (`ibm/granite-4-micro`), `size_bytes`
  becomes `sizeBytes` unchanged, `format` and `quantization.name` are reported
  as they come (`gguf`, `Q4_K_M`), `architecture` as `family`,
  `max_context_length` as `contextLength`, and the capability object is mapped
  onto the vocabulary the other backends use (`trained_for_tool_use` →
  `tools`, `vision` → `vision`, `reasoning` → `thinking`, on top of
  `completion`). Embedding models carry no capability object at all and are
  reported as `embedding`, listed like any other model — as on lemonade. The
  vision-language type (`vlm`) is a chat model: it is reported with
  `completion` plus whatever its capability object says, not as an embedding.
  `trained_for_tool_use` is the flag that matters for agents: LM Studio will
  accept `tools` for any model and emulate them through the prompt, but only
  a model trained for them calls them reliably.
- **Pull** — `POST /api/v1/models/download` takes an LM Studio hub reference
  (`lms ls` for what is already there) and answers with a job; the driver
  follows it through `GET /api/v1/models/download/status/{job_id}` and reports
  `downloaded_bytes` against `total_size_bytes`, so progress is real bytes. A
  model that is already downloaded reports complete straight away
  (`already_downloaded`), and a reference LM Studio does not know fails with
  `not_found`. On success the model is wired into kagent, as on ollama.
  One caveat the API forces: LM Studio does not promise that the `key` a
  download lands under is the reference that was asked for — the download
  answers no key, there is no resolve call, and Hugging Face artifacts are
  keyed per weight set (sometimes with an `@<quant>` suffix) while catalog
  models keep their canonical `publisher/model`. Since the ModelConfig is
  wired with the reference the caller gave, the pull resolves it against the
  library before reporting success and fails naming the mismatch, rather than
  leaving a ModelConfig whose model the server does not serve.
- **Load / unload** — `POST /api/v1/models/load` reads the weights and answers
  with an instance id; a load is bounded by ten minutes, not the usual call
  timeout. LM Studio evicts an *instance*, not a model, so unload looks the
  model's loaded instances up first; unloading a model that is not loaded is a
  no-op, as on ollama and lemonade. A **load** of an already-resident model is
  a no-op too, for the same reason the instance list exists: LM Studio loads
  an instance, not a model, so a repeated load would pin a second copy of a
  multi-GB model rather than answer "already loaded". Neither call takes a
  2xx as proof: both answers have to name the instance, because an LM Studio
  without these endpoints answers 200 with an error document. There is no
  keep-alive and no pinning.
- **Wiring** — a kagent `ModelConfig` named after the model
  (`ibm/granite-4-micro` → `ibm-granite-4-micro`) with `provider: OpenAI`,
  `openAI.baseUrl` = the agent host plus `/v1` (`--lmstudio-agent-host`,
  defaulting to the endpoint; reported as `agentEndpoint`) and the placeholder
  `OPENAI_API_KEY` Secret the kagent runtime insists on. LM Studio needs no
  key of its own.
- **Node** — none. LM Studio exposes no host hardware, so the driver reports
  `nodeInventory: false` and `GET /api/v1/nodes` answers `501 unsupported`.

Nothing here needs Kubernetes access beyond wiring, exactly as for ollama.

## The kserve backend

The driver consumes the `modelServing` contract of the
[`giantswarm/agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart, rendered by its `agent-platform-connectivity` chart:
the discovery ConfigMap `agent-platform-model-serving` (kind
`ModelServingConfig`) for the serving namespace, runtime, GPU resource name,
cache claim and preset selector, the GPU node pool's scheduling
(`spec.gpuPool`: the pool's taint and label) and the models Gateway
(`spec.gateway.endpoint`, `https://models.<domain>`: the origin every
`LLMInferenceService` is routed on at `/<namespace>/<name>`, which is how a
served model's address is known before KServe publishes it); the `ServingPreset` ConfigMaps
(`agent-platform.giantswarm.io/serving-preset=true`); the cache
PersistentVolumeClaim in the serving namespace. Every discovered value can be
overridden by a flag (`model-manager serve --help`, `--kserve-*`); the pool
scheduling by the registered backend document (`docs/backends.md`).

- **GPU node pool** — a pool created through the platform is tainted
  `nvidia.com/gpu` `NoSchedule` and labelled
  `giantswarm.io/machine-pool=<cluster>-<pool>`. The discovery ConfigMap's
  `spec.gpuPool.taint` / `spec.gpuPool.nodeSelector` (the chart's
  `modelServing.gpuPool.*`), or the backend document's `spec.kserve.gpuPool`,
  make the driver tolerate the taint on everything it schedules onto the pool
  — the composed predictors, the download Jobs and the inventory scan Jobs —
  and select the pool wherever the pod is not pinned to a node already. The
  descriptor (`GET /api/v1/backend`) reports it as `gpuPool`. Unset, nothing
  changes; the chart's DaemonSet takes its own `kserve.inventory.agent.*`.
  The document is read on the first call and cached for a minute; settings
  resolved while it was not published yet — the slice publishes it as it
  installs — stand for five seconds only, and a fit that finds no node while
  they lack it reads the document once more before answering: with the pool
  now known, the scale-from-zero path; still without it, `fits: false`,
  `retryable: true` and a reason naming the document (`the serving layer's
  discovery document <namespace>/<name> is not published yet — the slice is
  still installing; retry in a moment`), which `load_model` echoes in its
  refusal. `no accelerator node` is the verdict for a document that names
  no pool.

- **CPU presets** — a preset whose `resources.gpus` is `0` is CPU work: the
  fit judges every node of the cluster against its allocatable memory (the
  accelerator nodes and the CPU nodes alike, a node's `Eligible` rules as
  usual), and the preset knows no GPU pool and no accelerator RuntimeClass —
  its predictor carries neither the pool's toleration and label nor
  `runtimeClassName`, so on an installation whose GPU pool is tainted it
  lands on the cluster's CPU capacity. A bare model reference and every
  preset that requests a GPU keep the accelerator nodes as their capacity;
  `GET /api/v1/nodes` lists those.

- **Inventory** — the cache contents per node plus the InferenceServices of
  the serving namespace (readiness from conditions/`modelStatus`, node from the
  predictor pod, GPU request, predictor URL). The cache is read in one of two
  ways (`kserve.inventory.mode`): a **short-lived scan Job** per cache node
  mounts the claim read-only and walks `<claim>/<dir>` whenever the inventory
  is older than the TTL (default; never while the cache claim is unbound or
  the GPU pool has no node, and in the background when the caller's deadline
  leaves no room for a pod), or a **DaemonSet** of `model-manager
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
  when model-manager removes the directory. A record is bound to the cache it
  was made against (`claim`, `volume`): nothing is recorded while the serving
  layer has no cache (`cache.enabled: false` in the discovery document, a
  missing or unbound claim — the storage-initializer then fills storage that
  goes with the pod), `unload_model` drops a record that names a directory in
  no cache (bound to another claim or volume, or to none), and the ConfigMap
  goes with its last record. Without a marker or record the
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
  the discovery `nodeSelector` and the GPU pool's, carries no `NoSchedule` or
  `NoExecute` taint the pool toleration does not cover (a tainted pool node
  is capacity once its taint is the configured one), and can mount the cache
  claim whenever
  predictors mount it (cache enabled and `cache.redirectPolicy` on — the
  Kyverno rule that mounts the claim into every predictor) and the claim is
  pinned to nodes (a static local PersistentVolume or a `local-path` volume);
  a shared (RWX), unbound, missing or disabled cache never disqualifies a
  node. `eligibilityReason` names every failing rule (`not ready`, `outside
  the serving node selector (kubernetes.io/hostname=spark-8723)`, `taint
  nvidia.com/gpu:NoSchedule not tolerated (set the GPU pool taint)`, `cache
  claim hf-cache is pinned to spark-8723`). `pull`, `load` and `fit-check` refuse
  an explicit `node` that is not eligible with that reason (`412
  does_not_fit`; `fits=false` on the fit check) before any Job or
  InferenceService exists, and never pick an ineligible node themselves. A
  second node-local GPU node becomes a serving target only with per-node
  claims or shared storage — a chart decision, not a flag here. The claim
  has a say for the presets that store in it only — every download scheme
  (`hf://` and, with the redirect policy on, the rest) and `pvc://`. An
  `oci://` preset (KServe's modelcar: an OCI image containerd pulls onto the
  node's own disk; no storage-initializer, no claim mount) is judged without
  the claim's location: `fit-check` and `load` place it on any eligible GPU
  node with the budget, without a cache-node preference, and answer
  `cached: false` with `cacheSource: oci-image`; `pull` refuses it as
  `invalid` — the image is the platform's to pre-pull; nothing to download —
  and creates no Job. `nodes` still reports the pin: it describes the nodes,
  not a preset.
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
  mounts — reporting bytes on disk against the repository size. The Job
  downloads with `HF_HUB_DISABLE_XET=1` and `HF_HUB_ENABLE_HF_TRANSFER=1`:
  the Xet client connects to CDN addresses a Cilium `toFQDNs` DNS proxy never
  resolved for an allowed name and hangs on the dropped SYNs, hf_transfer
  resolves per connection through the system resolver, so every address is
  admitted. A download that writes nothing to its directory for
  `--kserve-download-stall-timeout` (`kserve.download.stallTimeout`, default
  10 min) fails with a reason the pull job shows — `DOWNLOAD STALLED: no bytes
  written to <dir> for <N>s (<bytes> bytes on disk, …)` — instead of hanging;
  partial files stay and a new `pull` resumes. Gated models
  need a token Secret (`--kserve-hf-token-secret`). The hub lookups of one
  fit check or search are bounded by `--kserve-hf-timeout` (`kserve.hf.timeout`,
  default 4 s): a hub that does not answer — egress blocked — lets `fit-check`
  fall back to the preset's requirements in time for the caller
  (`weightsSource: preset`; `reason` says the hub did not answer within the
  timeout), and a model no preset serves fails with that message. On a GPU
  pool with no node yet whose instance shapes the backend document lists
  ([docs/backends.md](docs/backends.md)) the check is against the pool's
  sizes, and the GPU memory budget is that of the GPUs the preset requests
  (`resources.gpus`, default 1) on a size — not the node's: a one-GPU preset
  on a four-GPU size has one card. A preset's declared weights
  (`requirements.weightsGiB`, what the pool was sized from) are answered as
  `declaredWeightsBytes` beside the hub-sized `weightsBytes`; when the hub
  holds more than the preset declares, `reason` says so on both verdicts
  (`…; the preset declares 15.0 GiB of weights, the Hub holds 24.6 GiB` on a
  fit; `…; the preset declares 15.0 GiB of weights; the Hub holds 24.6 GiB,
  which is what does not fit — correct the preset` on a refusal).
- **Serve / stop** — `load` composes the serving object from the preset
  after a fit check against the node's free budget; `unload` deletes it and
  unwires within the caller's deadline — the object is found by repository,
  object or preset name, never through the cache inventory, so a scan pod
  that cannot start never delays the deletion — and rescans the cache in the
  background where a scan pod may run, the answer's `inventory` saying
  whether one runs or why none can (the cache persists); `delete` removes
  the cache directory (refused while served). The kind is `--kserve-serving-kind` (`kserve.servingKind`):
  `auto` (default) composes an **`LLMInferenceService`**
  (`serving.kserve.io/v1alpha2`, the llm-d control plane) wherever that API
  is served and a classic `InferenceService` elsewhere; both kinds are
  listed, stopped and deleted, and a loaded model names its `kind`.
  - **`LLMInferenceService`, by spec shape**: `spec.model.uri` from the
    preset's `storageUri` (`hf://`, or `pvc://` into the cache),
    `spec.model.name` from `model.id`, `replicas: 1`,
    `router: {route: {}}` (KServe renders the `HTTPRoute` on the configured
    ingress gateway with the workload Service as its backend; the llm-d
    endpoint picker, `router.scheduler`, is opt-in through the backend
    document's `spec.kserve.router.scheduler` or a preset's
    `spec.router.scheduler`, and its `InferencePool` needs the Gateway API
    Inference Extension on the gateway — `docs/backends.md`),
    `template.containers[main]` with the preset's `args`,
    `env` and `resources` (GPU count under the discovery's resource name),
    `scheduling` as the template's `nodeSelector`/`tolerations` (merged with
    the discovery selector, the GPU pool's label and toleration, and the
    node pin), the chat template mounted, and
    **`template.runtimeClassName` from the discovery ConfigMap's
    `runtimeClassName`** when non-empty — absent when empty (the same rule
    as the classic predictor). **No `baseRefs`**: KServe chooses its
    well-known `LLMInferenceServiceConfig`s from the spec's shape and
    appends them itself; a preset that names a custom config
    (`spec.baseRefs: [{name: …}]`) is the only `baseRefs` case. **No
    image**: the container runs the image the well-known template names
    (`llm-d-cuda`, mirrored through the platform's registry override); a
    preset overrides it with `spec.template.containers[{name: main, image:
    …}]` when it has a reason — `spec.template` extras are copied on top,
    containers merged by name. model-manager creates and lists no
    `LLMInferenceServiceConfig`; `list_presets` lists one kind.
  - **`InferenceService`** (classic): `predictor.model` from the preset
    (runtime, format, storageUri, args, env, chat-template mount, GPU
    count); nodeSelector, the GPU pool's label and toleration,
    runtimeClassName, deployment strategy and timeout from discovery;
    `spec.predictor` extras verbatim.
- **Wiring** — in the load call, before the model is ready, a kagent
  `ModelConfig` **named after the InferenceService** (`provider: OpenAI`,
  `baseUrl` = the model's address + `/v1`, `model` = the served model name:
  the InferenceService name, which the ClusterServingRuntime serves under
  `--served-model-name {{.Name}}`, or an LLMInferenceService's
  `spec.model.name`) — the same rule the portal's serve flow applies; the
  `load` answer's `wiring` names it, the `load` job refreshes it from the
  address KServe publishes once the model is ready, and a served model
  model-manager manages that has none is wired by the caller's next
  `list_loaded_models` (`wiring.reason: "wired on read"`). The address is the
  one KServe published or, until then, the one the object is expected on: the
  discovery document's models Gateway (`spec.gateway.endpoint` +
  `/<namespace>/<name>`) for an `LLMInferenceService`, the in-cluster Service
  otherwise. The API key follows the address: a model **routed on the models
  Gateway** (an external host) gets `apiKeyPassthrough: true` and no Secret —
  the agent forwards the person's own token, the only thing the Gateway's JWT
  policy admits, so a placeholder key would fail every turn with 401; a model
  reached on its **in-cluster Service** gets the placeholder
  `OPENAI_API_KEY` Secret the go ADK runtime insists on, which keyless vLLM
  never checks. A re-wire that moves a ModelConfig onto the Gateway removes
  its placeholder Secret. A ModelConfig that already points at the predictor (same
  host, same served model name), whoever created it, counts as the model's
  wiring: it is reported with `managed: false`, never duplicated and never
  deleted; `unwire`/`unload` only remove ModelConfigs model-manager created.
- **Ownership** — InferenceServices in the serving namespace that
  model-manager created or that carry the `agent-platform.giantswarm.io/preset`
  label (the portal's serve flow) can be unloaded here; hand-written ones are
  inventory only (`409 conflict` on unload; `managedBy` says who owns them).
- **State** — the loaded models (`GET /api/v1/loaded`, `list_loaded_models`)
  are every InferenceService and LLMInferenceService of the serving namespace
  whatever their readiness: `status` `Ready`, `Pending` (the predictor pod
  waits for a node, the weights or an image — `reason` `Unschedulable` with
  the scheduler's message, `DownloadingWeights`, `ImagePullBackOff`),
  `NotReady` (the Ready condition's `reason`, a failed load) or
  `Terminating`, with `message` in words. `unload` takes the preset name,
  the object name or the repository id and deletes the object in any state;
  its answer says `status: Terminating` and that the list shows the object
  until it is gone. A `load`'s answer names the object it created
  (`running.resource`, `running.kind`), carries the fit verdict it was
  judged by (`fit`, the `check_fit` shape) and the initial `running.steps`;
  a refusal (`412 does_not_fit`) carries the fit's `reason`, the
  declared-weights clause included.
- **Phases** — `phase` refines `status` with where a serve is, and `steps`
  is the whole timeline: one entry per phase in order, each
  `{name, state: pending|inProgress|done|failed, since, finishedAt, reason,
  message}` with `since`/`finishedAt` from the pod's conditions, container
  statuses and Events (Karpenter's nomination, the kubelet's `Pulling`/
  `Pulled` with the pull's duration, probe failures), so a caller shows
  elapsed time per step. The phases of a fresh serve on a scale-to-zero pool
  (proof 1: ≈ 12 min to Ready):

  | `phase` | What is happening | Read from |
  |---|---|---|
  | `scheduling` | no node has a free GPU; the autoscaler is asked and, once it nominated a NodeClaim, until that claim has launched an instance | `PodScheduled=False` (`Unschedulable`), `FailedScheduling` events; Karpenter's `Nominated` event names the NodeClaim (`karpenter.sh/v1`, read as the caller): `Launched` not yet `True` keeps the step with `reason: NodeLaunching` (`Karpenter nominated NodeClaim gpu-l4-x7k2q; no instance has launched yet`); `Launched=False`, or an `InsufficientCapacityError` / `NodeClassNotReady` Warning event on a NodeClaim of the pool (in the `default` namespace, where a cluster-scoped object's events land — Karpenter deletes a refused claim within seconds and nominates another) gives `reason: CapacityUnavailable`. A capacity refusal is read whole: what the claim asked for — its `spec.requirements` while it stands, else the pool's template (the `karpenter.sh/v1` NodePool it is named after, read as the caller; the pool chart names the family and the sizes, composed to types) — and every refusal text (the claim's `Launched=False`, the events) for the sizes and the zone the cloud names and the zones it names as having capacity where the text is whole (Karpenter's event holds the first 300 characters of the cloud's answer: the first size and the zone; the claim's condition holds it whole), plus whether the serving namespace's cache claim is bound to a volume in that zone — the pool's pin, cluster-manager's `pool.zones`: `Karpenter could not launch a node: 4 NodeClaims refused, the last (gpu-l40s-46flj) at 2026-09-18T12:24:14Z — InsufficientCapacityError: the cloud has no g6e.2xlarge, g6e.4xlarge or g6e.8xlarge capacity in eu-central-1b, the zone the pool is pinned to by the cache claim hf-cache (its volume lives there); it has capacity in eu-central-1a and eu-central-1c. The way out is a pool in eu-central-1a or eu-central-1c (create_node_pool with zones naming it and cache: false), or removing the cache claim so the next pool is not pinned; Karpenter retries while the pod waits`. A pool that pins no zone lets a claim launch in every zone of its node class's subnets, and a fleet that launched nothing was refused them all: the zones are the claim's own zone requirement while it stands, else the node class's (`ec2nodeclasses.karpenter.k8s.aws`, read as the caller), `requestedZones` names them all, `availableZones` never a zone the same answer refused, `pinnedByCache` is false, and the message reads `the cloud has no g6e.2xlarge, g6e.4xlarge or g6e.8xlarge capacity in any zone the pool allows (eu-central-1a, eu-central-1b and eu-central-1c). No zone is left to move to; wider sizes or another accelerator (a re-run of create_node_pool) give Karpenter more to choose from, and it retries while the pod waits` (the node class unreadable: the zone the cloud named, `at least`). The step carries the same as fields — `refusedInstanceTypes`, `requestedZones`, `availableZones`, `pinnedByCache` (absent when the cache claim or its volume could not be read) — and the model's `reason`/`message` carry the message. A refusal that is not the cloud's capacity wording keeps Karpenter's words; a read that fails — the claim, the events, the NodePool, the node class, the cache claim — is named in the message, never silent; ≈ 30 s from nomination to launch |
  | `nodeStarting` | the node exists — an instance launched — and its GPU is not allocatable yet | the NodeClaim's `Launched=True` (`since` = its transition; the message names the node once the claim registered it), a `nominatedNodeName`, the pod bound; the node's allocatable `nvidia.com/gpu`; ≈ 3.5 min until the node registers, ≈ 1 min more for the GPU |
  | `downloadingWeights` | the `storage-initializer` fills the cache directory | the init container; `bytesTotal` (the preset's weights), `bytesCompleted` (a bounded cache-agent scan while filling, cache-agent mode only), `cached: true` when it finished within 15 s — the claim held the weights (72 s for 8 GB, else 0.3 s) |
  | `pullingImage` | the kubelet pulls the runtime image | the container `Waiting` (`ContainerCreating`), `Pulling`/`Pulled` events (the message carries the duration); ≈ 4 min |
  | `loading` | vLLM loads the weights until the startup probe passes | the container `Running`, not `Ready`; `Unhealthy` events say what the probe saw; ≈ 1 min. A runtime that died and is restarted by the kubelet keeps the step under way with `reason: CrashLoop` and a message naming the crash count, the exit code and the last error line of the crashed container's log (`kubectl logs --previous`, read as the caller): `runtime crashed 2× (exit 1): PermissionError: [Errno 13] Permission denied: '/mnt/models/.cache/vllm'`; a caller who may not read `pods/log` gets the message with the reason the line is missing |
  | `routing` | the pod is ready, KServe resolves the route | `Ready=False` `HTTPRoutesNotReady` |
  | `ready` | the endpoint answers | `Ready=True` (`since` = its `lastTransitionTime`) |
  | `failed` | a step failed — the step says why | a capacity refusal standing when the GPU pool's scale-up budget is spent — `--kserve-scale-up-timeout` / `KSERVE_SCALE_UP_TIMEOUT` / chart value `kserve.scaleUpTimeout`, default `10m`, counted from the pod's creation: the `scheduling` step fails with `CapacityUnavailable`, its message plus `; no node came within the scale-up budget of 10m0s`, and the model's `reason`/`message` carry it (a nominated claim still launching without a refusal never fails the step); `ImagePullBackOff`/`ErrImagePull`; `CrashLoopBackOff` (the kubelet backed off from restarting a runtime that keeps dying: the `loading` step fails with the crash message and the back-off, and the model's `reason`/`message` carry it); an initializer that exited non-zero, restarted or ran longer than 30 min (`DownloadStalled`); `modelStatus.lastFailureInfo` |
  | `terminating` | the object is being deleted | `deletionTimestamp`; the steps stay as they were |

  Without a pod yet the object is `scheduling` (`WaitingForPod`) since its
  creation; a Ready object whose pod the driver cannot see has every step
  done. While the pod has no node, Karpenter's account of the one it waits
  for — the claim launching, its refusal, the instance registering — is the
  model's `reason`/`message` in `list_loaded_models` instead of the
  scheduler's `Unschedulable`; a caller who may not read
  `nodeclaims.karpenter.sh` or the `default` namespace's events gets the
  step's message with the reason the read failed, never silently. Backends
  without a serve lifecycle (ollama, lemonade, lmstudio) answer neither
  `phase` nor `steps`. Reading the Events and the nodes costs one bounded
  (3 s) list per pod — plus, for a pod without a node, the nominated
  NodeClaim and Karpenter's Warning events — concurrently, so three served
  models stay inside a caller's ~10 s deadline.
- **Cache verdict** — `check_fit`'s `cached` is a boolean and `cacheSource`
  says how it was decided: `scan` (a cache scan answered in this call),
  `index` (no scan could run — a GPU pool at zero, the caller's deadline —
  and the cache index remembers a directory an InferenceService filled for
  the repository in the claim and volume bound now; a record bound to another
  cache, or to none, is no verdict), `unknown` (neither answered; `cached: false` is then no
  verdict), `oci-image` (the preset serves an OCI model image the nodes pull
  themselves; nothing of it is in the claim, and `cached: false` is the
  verdict). On the scale-from-zero pool path the claim is asked as a whole,
  so weights an earlier serve left in it answer `cached: true`.

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

Without `--backend`/`--backends` the process starts with no backend and waits for backend
documents in `--namespace` (`POD_NAMESPACE`); see [docs/backends.md](docs/backends.md). A static
backend:

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

Against an LM Studio on the host:

```sh
model-manager serve \
  --backend lmstudio \
  --lmstudio-endpoint http://127.0.0.1:1234 \       # as reached by model-manager
  --lmstudio-agent-host http://172.21.0.1:1234 \    # as reached by agent pods (+ /v1 in ModelConfigs)
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
retried by the caller. A restart of the process loses the in-memory jobs and
nothing else: a served model's ModelConfig is written by the load call itself
and, when missing, by the caller's next `list_loaded_models`, and a running
download Job is joined by the next `pull_model`. Health endpoints (`/healthz`,
`/readyz`, `/backendz`) and the OAuth metadata stay public.

## Helm chart

`helm/model-manager` — see its [README](helm/model-manager/README.md). Keys the
[`giantswarm/agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart sets for its `model-manager` component: `backend`, `ollama.endpoint`,
`ollama.agentHost`, `lemonade.endpoint`, `lemonade.agentHost`,
`lmstudio.endpoint`, `lmstudio.agentHost`, `kagent.namespace`, `mcp.enabled`, `oauth.*`,
`muster.mcpServer.*`; for kserve `kserve.namespace` (the serving namespace),
`kserve.discovery.*`, `kserve.hf.tokenSecret.*` and the `kserve.*` overrides. Optional, off by default: `muster.mcpServer.enabled`
(renders an `mcpservers.muster.giantswarm.io` CR), `httpRoute.enabled`,
`networkPolicy.enabled`, `oauth.enabled`.

## Development

See [docs/development.md](docs/development.md).
