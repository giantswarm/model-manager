# Backends registered at runtime

model-manager ships with every installation and starts with **no backend**. A backend — Ollama, LM
Studio, Lemonade or KServe — is registered at runtime by writing a **backend document**: a
ConfigMap in model-manager's namespace that model-manager finds by label and validates when it
reads it. This page is the contract: what the `add_backend` tool writes, what cluster-manager
writes when it creates a GPU node pool, and what the Dev Portal's *Add model backend* page renders.

## The document

| | |
|---|---|
| Object | `ConfigMap` in model-manager's own namespace (`POD_NAMESPACE`; the chart's release namespace) |
| Label | `agent-platform.giantswarm.io/model-backend: "true"` — model-manager watches this selector |
| Name | `model-backend-<name>`, the backend's name (`metadata.name`, default `spec.kind`) — `model-backend-ollama`, `model-backend-lmstudio`, `model-backend-lemonade`, `model-backend-kserve`, and `model-backend-kserve-<cluster>` for each further serving cluster; one document per backend |
| Key | `backend.yaml` |
| Document | `apiVersion: agent-platform.giantswarm.io/v1alpha1`, `kind: ModelBackend` |
| Informational label | `agent-platform.giantswarm.io/model-backend-source: person\|cluster-manager` (repeats `spec.source`) |

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: model-backend-kserve
  namespace: agent-platform
  labels:
    agent-platform.giantswarm.io/model-backend: "true"
    agent-platform.giantswarm.io/model-backend-source: cluster-manager
data:
  backend.yaml: |
    apiVersion: agent-platform.giantswarm.io/v1alpha1
    kind: ModelBackend
    metadata:
      name: kserve
    spec:
      kind: kserve
      source: cluster-manager
      kserve:
        target:
          cluster: gpu01
          organization: giantswarm
          apiServer: https://api.gpu01.example.com:6443
          caBundle: |
            -----BEGIN CERTIFICATE-----
            ...
            -----END CERTIFICATE-----
          servingNamespace: model-serving
        discovery:
          namespace: model-serving
          name: agent-platform-model-serving
        gpuPool:
          taint:
            key: nvidia.com/gpu
            effect: NoSchedule
          nodeSelector:
            giantswarm.io/machine-pool: gpu01-gpu-l4
```

### One kserve backend per serving cluster

An installation serves models on its own cluster and on any number of workload clusters, each
with its own GPU pools. cluster-manager registers one kserve backend per serving cluster: `kserve`
(ConfigMap `model-backend-kserve`) for the installation's own cluster and `kserve-<cluster>`
(ConfigMap `model-backend-kserve-<cluster>`, `metadata.name: kserve-<cluster>`) for a workload
cluster. Each is a backend of its own with its own target, discovery, `gpuPool` / `gpuPools`,
inventory and presets; the tools address one by name through their `backend` argument
(`check_fit`, `load_model`, `pull_model`, `unload_model`, ...), and a model several of them hold
is ambiguous until the call names one. Models, jobs, nodes and the ModelConfigs model-manager
wires carry the backend's name. Deleting one document drops that backend alone.

### Schema (enforced when the document is read)

| Field | Required | Meaning |
|---|---|---|
| `apiVersion` | yes | `agent-platform.giantswarm.io/v1alpha1` |
| `kind` | yes | `ModelBackend` |
| `metadata.name` | no | The backend's name, defaulting to `spec.kind`: equals `spec.kind`, or for `kserve` is `kserve-<cluster>` (a DNS label). The ConfigMap is named `model-backend-<name>` |
| `spec.kind` | yes | `ollama` \| `lmstudio` \| `lemonade` \| `kserve` — the driver |
| `spec.source` | yes | `person` \| `cluster-manager` — who wrote it; `static` is reserved for `--backends` and refused |
| `spec.endpoint` | host kinds | Base URL as reached by model-manager (`http(s)://host:port`); not accepted for `kserve` |
| `spec.agentEndpoint` | no | Base URL as reached by agent pods; defaults to `endpoint` |
| `spec.credentials.secretRef.{name,key}` | no | A Secret holding a token; `key` defaults to `token`. Accepted for `kserve` — the Hugging Face hub token, a Secret in the serving namespace. The host drivers present no bearer yet and refuse it by name |
| `spec.kserve` | kserve | Required for `kserve`, not accepted otherwise |
| `spec.kserve.target.cluster` | no | Target cluster; `local` (default) is the cluster model-manager runs on |
| `spec.kserve.target.organization` | no | The target's organization; reported with the cluster as `target` on the backend, its models, nodes and presets |
| `spec.kserve.target.apiServer` | remote | The target apiserver URL; required unless `cluster` is `local`, not accepted for `local` |
| `spec.kserve.target.caBundle` | remote | The target apiserver's CA, PEM; required unless `cluster` is `local` |
| `spec.kserve.target.servingNamespace` | yes | Namespace on the target holding the LLMInferenceServices, download Jobs and the cache |
| `spec.kserve.discovery.namespace` | no | Where the model-serving discovery ConfigMap (`kind: ModelServingConfig`) lives on the target; defaults to `servingNamespace` |
| `spec.kserve.discovery.name` | no | Its name; defaults to `agent-platform-model-serving` |
| `spec.kserve.gpuPool` | no | The GPU node pool's scheduling; replaces the discovery ConfigMap's `spec.gpuPool` (see below) |
| `spec.kserve.gpuPool.taint.{key,value,effect}` | no | The pool's taint (`key` required; `effect` `NoSchedule` \| `PreferNoSchedule` \| `NoExecute`, empty for every effect). Tolerated by the inventory scan pods, the download Jobs and the predictors model-manager composes: `value` empty tolerates every value (`Exists`), set compares it (`Equal`) |
| `spec.kserve.gpuPool.nodeSelector` | no | The pool's label(s) (`giantswarm.io/machine-pool: <cluster>-<pool>`), the node selector of the composed predictors and of every scan pod and download Job that is not pinned to a node |
| `spec.kserve.router.scheduler` | no | `true` composes the llm-d endpoint picker (`router.scheduler`) beside the route on every `LLMInferenceService` the backend composes; default `false`, the route alone — KServe routes the models Gateway to the workload Service (see below). A preset's `spec.router.scheduler` overrides it for that preset |
| `spec.kserve.gpuPool.instances[]` | no | The sizes the pool launches, in the shape cluster-manager's `create_node_pool` answer lists under `sizes`; the fit check judges a model against them while the pool has no node (see below). Every entry: `instanceType` (required, `g6.xlarge`), `size` (`xlarge`; defaults to the part of `instanceType` after the family), `vcpu`, `memoryGiB`, `gpus`, `gpuMemoryGiB` (the memory of one GPU) — positive integers — `usableVcpu`, `usableMemoryGiB` (positive numbers: what a node of the size leaves a predictor after the kubelet's reservations and the fleet's daemonsets; a `g6.xlarge` 3 / 11.9 of 4 / 16) and optionally `computeCapability` (the GPU's CUDA compute capability as `major.minor`, `"8.6"`; the fit check judges the GPU generation a model's weights need against it, and without it against the instance family's) |
| `spec.kserve.gpuPools.<pool>.instances[]` | no | The cluster's GPU pools by name — the value of their nodes' `giantswarm.io/machine-pool` label — each with its sizes in the `instances[]` shape above, which cluster-manager writes while the cluster has two or more pools and so no `gpuPool` selector pins every predictor. The fit check places a model on the chosen node's pool, or, when no node hosts it, on the pool with no node yet whose smallest hosting size is the smallest; `load_model` pins the predictor to that pool (`giantswarm.io/machine-pool=<pool>` with the pools' taint) and `list_loaded_models` names it as `pool` |

Unknown fields are refused. A document that fails the schema is **reported and not loaded**: it
appears under `invalid` in `list_backends` with the ConfigMap name and the failing field
(`spec.endpoint: required for ollama`), and the process keeps running with the backends it has.
A loaded document that breaks — it fails the schema, or its backend fails to build — drops its
backend in the same step that reports it: what serves is always what the ConfigMap holds, never
the last good document beside the report. Fixing the ConfigMap loads it; deleting it clears the
report.

The target **never carries credentials**: every Kubernetes call the kserve backend makes presents
the caller's own token (`--downstream-oauth`, the platform default), so the target apiserver must
trust the installation's Dex (the cluster chart's OIDC / `structuredAuthentication` values). A `local`
target uses model-manager's in-cluster address and CA. For a remote target:

- **Refused when read** when `--downstream-oauth` is off (every call there would be anonymous) and
  with `--kserve-inventory-mode=daemonset` (it dials the cache agents' pod IPs, which the
  installation cannot reach; the `pod` mode reads its scan pod through the target apiserver). A new
  document is reported under `invalid` and not loaded, and `add_backend` answers `registered: false`
  with the `error`. A registered kserve document edited into a refusal — pointed at a remote target
  while `--downstream-oauth` is off, or given the `daemonset` inventory — is a document whose
  backend fails to build: its backend is dropped in the same step that reports it, so
  `list_backends` shows the document under `invalid` and no kserve backend, never the last good
  target beside the report.
- **A refusal names its precondition.** A 401 from the target says the target apiserver must trust
  the installation's Dex as an OIDC issuer, naming the issuer and audience of the caller's token — or
  that the token expired; a 403 names the caller's RBAC on the target, or a call that carried no
  caller's token. The code and reason stay the apiserver's.
- **Detached work runs as the caller.** A background cache rescan and the cleanup of a cancelled
  download present the token of the call that caused them; with no caller they are skipped (logged
  once, and the answer's `inventory.reason` says why), never made anonymously. A caller the target
  refuses is not retried without the token.
- **The model server is read through the target apiserver.** The workload Service's cluster-local
  name resolves only on the target, so `GET /version`, `GET /openapi.json` and the first request of
  each Ready transition go through the Service proxy
  (`/api/v1/namespaces/<ns>/services/http:<name>-kserve-workload-svc:8000/proxy/<path>`) as the
  caller, who needs `get` and `create` on `services/proxy` in the serving namespace. A refusal keeps
  the serve in `routing` with the target's message and is read again a minute later. The local
  cluster reads the Service directly.
- **Agents reach a model on the target's models Gateway.** The backend's `agentEndpoint` is the
  gateway origin from the target's discovery ConfigMap (`spec.gateway.endpoint`,
  `https://models.<cluster>.<domain>`); every ModelConfig names
  `<origin>/<namespace>/<model>/v1` with `apiKeyPassthrough: true` and no placeholder Secret. A target
  whose discovery document enables no Gateway says so in the backend's `message`: a model served on
  its cluster-local address is out of the installation's reach. The kagent wiring itself stays on the
  installation's cluster.

Everything the document does not name — images, timeouts, inventory mode, the cache claim — keeps
the value of the chart's `kserve.*` values (the flags), which double as defaults for a registered
kserve backend.

### The GPU node pool input

A GPU node pool created through the platform arrives tainted `nvidia.com/gpu` `NoSchedule` (only
accelerator work lands there) and labelled `giantswarm.io/machine-pool=<cluster>-<pool>`. The
kserve backend reads both as **one input, `gpuPool`**, from the discovery ConfigMap first — the
`ModelServingConfig` document's `spec.gpuPool.taint` and `spec.gpuPool.nodeSelector`, which the
platform chart renders from its `modelServing.gpuPool.taint` and `modelServing.gpuPool.nodeSelector`
values — and from the registered document's `spec.kserve.gpuPool` second: a `taint` in the document
replaces the discovery's taint, a `nodeSelector` in the document the discovery's selector. The
serving runtime's and the presets' tolerations are the chart's; this input covers the three things
model-manager schedules itself:

| Object | Toleration | Node selector |
|---|---|---|
| Composed `LLMInferenceService` / `InferenceService` predictor | first, the preset's `scheduling.tolerations` after it (a repeated entry once) | merged under the preset's `scheduling.nodeSelector` and the node pin |
| Download Job | always | only when the Job is not pinned to a cache node (a shared cache); a pinned Job keeps its node |
| Inventory scan Job (`kserve.inventory.mode: pod`) | only while a pool node exists (a scan never launches one) | likewise; the chart's DaemonSet (`daemonset` mode) takes `kserve.inventory.agent.tolerations` / `.nodeSelector` |

`GET /api/v1/nodes` reports a tainted GPU node as serving capacity once the taint is tolerated;
before, its `eligibilityReason` names the taint (`taint nvidia.com/gpu:NoSchedule not tolerated
(set the GPU pool taint)`), and a node outside the pool's selector says so. `GET /api/v1/backend`
reports the input as `gpuPool`. Unset on both sides, nothing changes.

#### The pool's instance shapes and the fit check

A pool the autoscaler runs at scale-to-zero has no node until a predictor is Pending — so
`check_fit` has no node to judge against. Without more, it answers `fits: true`,
`budgetSource: pool-scale-from-zero` and a reason that says the fit is unverified. A pool whose
nodes are all still **starting**, or a cluster whose only matching nodes are, — created within the last 15 minutes and kept from serving only by
not being ready and by start-up taints (`<driver>/agent-not-ready` such as
`ebs.csi.aws.com/agent-not-ready`, `node.kubernetes.io/not-ready`,
`node.cloudprovider.kubernetes.io/uninitialized`, `karpenter.sh/unregistered`) — is judged the same
way: the reason names the node and what it still waits on, and the predictor schedules once the
taints are lifted. An older node with those taints is broken, not starting, and keeps the refusal.
With the pool's shapes known, a pool that has nodes is judged against its sizes too whenever **none
of its nodes takes the predictor**: every node is starting or no serving target (a node Karpenter
disrupts: not ready, `karpenter.sh/disrupted`), or has fewer GPUs than the preset requests (a
one-GPU node for a four-GPU preset). The predictor would sit Pending beside those nodes, and
Karpenter launches a size for it; the reason names each node and what keeps it from taking the
predictor (`no node of the GPU pool (…) takes the predictor (node … has 1 GPU, the predictor
requests 4): the pool launches one — the node comes as 12xlarge …`), followed by the ready node's
own verdict. A preset a node takes is judged against that node, as before.
Without shapes, Karpenter is
then the first to know whether any size of the pool can host the predictor, in its own log. With
the pool's **instance shapes** — `gpuPool.instances`, the same list on the document and on the
discovery ConfigMap, the document's replacing discovery's — the check judges the model first:

- the preset's `resources.requests` `cpu` and `memory` against a size's `usableVcpu` /
  `usableMemoryGiB` (a request the preset does not name is zero), the preset's `gpus` (default 1)
  against the size's, and the weights plus overhead the check sized against the memory of the GPUs
  the preset requests on the size — `gpuMemoryGiB × the preset's gpus`, not the node's: a one-GPU
  preset on a four-GPU size has one card (the arithmetic cluster-manager sized the pool with);
- the **smallest size that hosts it** is the node it will come as: `fits: true`, `instanceType`
  names the size (`g6.xlarge`), `budgetBytes` is the memory of the requested GPUs on it, and the
  reason says the size's leftovers;
- when **no size hosts it**: `fits: false`, the reason names the pool's sizes, what the predictor
  asks and what the largest size leaves it — and `load_model` / `pull_model` refuse with that
  reason before any object is created, instead of a predictor that can only sit Pending;
- a preset's **declared weights** (`requirements.weightsGiB`, what the pool was sized from) are
  answered as `declaredWeightsBytes` beside the Hub-sized `weightsBytes`; when the Hub holds more
  than the preset declares, the reason says so on both verdicts — `the preset declares 15.0 GiB of
  weights, the Hub holds 24.6 GiB` on a fit, `…; the Hub holds 24.6 GiB, which is what does not
  fit — correct the preset` on a refusal — and a declaration that covers the Hub adds nothing;
- a model **without a preset** is judged on its weights and the default overhead against the GPU
  memory alone;
- a size hosts the model only when vLLM's **KV cache** holds one sequence of the preset's
  `--max-model-len` on one of its GPUs (`gpuMemoryGiB` is the nominal size a card is sold as, in
  decimal GB: an L40S's 48 are 44.7 GiB), the same check as on a node (see the README's Import),
  and the reason names the KV need and what the size leaves it.

The document is what tells the fit there is a pool at all. Settings resolved while the discovery
ConfigMap was not published yet — the serving slice publishes it as its connectivity child installs —
stand for five seconds, not the minute the document's settings stand, and a fit that finds no
candidate node while its settings lack the document re-reads it once before answering: with a pool
named, the scale-from-zero verdict above; still without it, `fits: false`, `retryable: true` and
`reason: the serving layer's discovery document <namespace>/<name> is not published yet — the slice
is still installing; retry in a moment`, which `load_model` / `pull_model` echo in their refusal.
A document that names no pool while the pool's instance shapes are known — cluster-manager registers
them with a new pool, and the slice names the pool in the document when its connectivity child renders
it again, moments later — is treated the same way: its settings stand for five seconds, a fit that
finds no candidate node re-reads it once, and while it still names no pool the answer is `fits: false`,
`retryable: true` and `reason: the serving layer's discovery document <namespace>/<name> names no GPU
pool yet (no spec.gpuPool.nodeSelector) though the pool's instance shapes are known — the slice is
still publishing the new pool; retry in a moment`. A load composes its predictor with the settings the
fit was judged on, the pool's taint and label included.
`no accelerator node` is the verdict for a document that names no pool while no pool shapes are known.

A list with an invalid entry is refused on the document (the document is reported and not loaded)
and ignored from discovery (the answer is the unverified one). Once a node of the pool exists the
fit is against that node again, as before.

### The route and the endpoint picker

Every `LLMInferenceService` the backend composes asks KServe for a route, `spec.router.route`, and
KServe renders the model's `HTTPRoute` on the models Gateway with the **workload Service**
(`<name>-kserve-workload-svc:8000`) as the backend of every `/v1/*` rule: `ResolvedRefs=True` as
soon as the Service exists, `Ready=True` once the predictor is up, nothing else in the path. This
is the default shape and the one a single-replica predictor needs; the Gateway's JWT policy stays
the one boundary in front of the model.

`router.scheduler` asks KServe for the **llm-d endpoint picker** beside the route: a
`<name>-kserve-router-scheduler` Deployment and an `InferencePool`, and the `HTTPRoute`'s `/v1/*`
rules then target the `InferencePool` (`inference.networking.k8s.io`) instead of the Service. A
gateway resolves an `InferencePool` backendRef only with the **Gateway API Inference Extension**;
without it the route stays `ResolvedRefs=False BackendNotFound`, the object never becomes Ready
(`reason: HTTPRoutesNotReady` on the loaded model) and the model has no route. The picker buys
something only with several replicas — prefix-cache-aware routing, prefill/decode disaggregation —
so it is **opt-in**: `spec.kserve.router.scheduler: true` on the document switches it on for every
preset, `spec.router.scheduler: true|false` on a preset for that preset alone (the preset's value
wins). A shape that switches it on needs the Inference Extension enabled on the models Gateway
(giantswarm/agent-platform#504). The shape is composed at load: an object composed before a change
keeps its shape until it is unloaded and loaded again.

### Tracing

A served model exports no traces unless its load asks for them: `load_model` (`POST
/api/v1/models/load`) takes `tracing: true`, which composes `spec.tracing: {}` on the
`LLMInferenceService`. KServe's controller appends its tracing `LLMInferenceServiceConfig`
(`kserve-config-llm-tracing`) to an object whose spec carries `tracing`, whatever the spec says, so
the exporter, the platform's OTLP endpoint, the sampler and the pod labels the OTLP gateway routes
by are the platform's, never a value of the caller's: the model's spans land in the platform's trace
store under the caller's trace. Off by default — detailed vLLM traces cost throughput. The served
model reports the switch as `running.tracing` (`list_loaded_models`: `tracing`). It is composed at
load like the rest of the shape: a preset already served with the other setting is refused with
`conflict` (`already serves with tracing off, not on; stop it first`), since switching it restarts
the model.

### The API interfaces of a served model

Which APIs a served model answers is read from its running server, never declared: once each time
an `LLMInferenceService` turns Ready the backend asks its workload Service
(`<name>-kserve-workload-svc:8000`) for `GET /version` and `GET /openapi.json`, and reports
`runtime {name: vllm, version}` and `interfaces [{type, path}]` on the loaded model
(`list_loaded_models`, `GET /api/v1/loaded`, and `running` of `list_models`). Since vLLM 0.16 the
OpenAI-compatible server registers a family of routes only when the model's task includes it, so
the route list is the interface list: a generate model answers `Completions`
(`/v1/chat/completions`, the legacy `/v1/completions` with it), `Responses`, `Messages` and
`AnthropicTokenCount` (`/v1/messages/count_tokens`), a pooling model `Embeddings` and no chat route.
The names are agentgateway's format vocabulary, the one an `AgentgatewayModel`'s `custom.formats`
takes. A preset states no interfaces.

The same read asks the model its first request, because a runtime can pass its readiness
(`GET /health`) and fail every request: a chat completion of one token under the served name for
a generate model, one embedding for a pooling model. The serve is `ready` only once that request
was answered; an error answer fails the `ready` step with the runtime's status and first error
line, and no answer keeps it `routing` and asks again after a minute. It is sent once per Ready
transition, to the workload Service the interface reads use, so model-manager's reach to the
model's port (the serving namespace's network policy) is what the reads already need.

A server that publishes no route list (a runtime started with `--disable-fastapi-docs`, which the
agent-platform chart refuses in a preset), one whose list names `/v1/chat/completions` and
`/v1/embeddings` side by side (a server that registers every route whatever the model serves: vLLM
before 0.16, whose handlers refuse the other family at request time) or a server that could not be
read reports `interfaces: []` and `interfacesReason`. Nothing is judged by the version: the llm-d
runtimes' vLLM is a source build that reports `0.1.dev1+g<commit>`, and the version is reported as
the server gives it. A read that got no answer — the serving
namespace's network policy dropping model-manager's connection, a runtime failing — is repeated
after a minute; an answer stands until the model turns Ready anew.

Whether a registered interface serves a given request is the preset's business: tool calling
needs `--enable-auto-tool-choice` and a `--tool-call-parser`, thinking blocks a
`--reasoning-parser`.

### Served models on the LLM endpoint

The platform's LLM endpoint is described by an **LLM endpoint document**: a ConfigMap in
model-manager's own namespace (`--namespace`, the backend documents' namespace) labelled
`agent-platform.giantswarm.io/llm-endpoint=true`, whose key `llm-endpoint.yaml` holds

```yaml
apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: LLMEndpoint
spec:
  parentRefs:                 # Gateway API parents every served model attaches to
    - {group: gateway.networking.k8s.io, kind: Gateway, name: agentgateway, sectionName: llm}
  endpoint: http://agentgateway.agent-platform.svc:8081   # the listener's in-cluster URL
  externalEndpoint: https://llm.example.io                # optional: the endpoint's public URL
```

The agent-platform chart renders it in the platform release with `llmRouting` on and
model-manager serving. It is the platform's and not a serving slice's: a GPU node pool's slice is
a release of its own, and the endpoint's data plane reaches the models of its own cluster only.
model-manager reads it with its ServiceAccount, every time it resolves its serving settings, for a
kserve backend serving model-manager's own cluster: a backend with a remote target reads none. A
parent without a namespace is in the document's. With exactly one document, every Ready model
created from a preset whose server registered at least one interface goes on the endpoint as two
`AgentgatewayModel`s. Two or more documents, or one that does not parse, put nothing on it, and
the log says why. The objects live in the parents' namespace and are written with model-manager's
own ServiceAccount (the chart grants it `agentgatewaymodels` there):

```yaml
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayModel
metadata:
  name: <preset>-workload         # the concrete model: the served workload
  labels: {app.kubernetes.io/managed-by: model-manager, model-manager.giantswarm.io/backend: kserve, agent-platform.giantswarm.io/preset: <preset>}
spec:
  parentRefs: [<the document's parentRefs>]
  match: {model: <Hugging Face id>}
  visibility: Internal
  provider: Custom
  baseURL: http://<name>-kserve-workload-svc.<namespace>.svc.cluster.local:8000/v1
  custom:
    formats: [<the interfaces the server registered>]
---
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayModel
metadata:
  name: <preset>                  # the public name: what a client sends as `model`
  labels: {…the same…}
spec:
  parentRefs: [<the document's parentRefs>]
  virtualModel:
    weighted:
      targets: [{modelRef: {name: <preset>-workload}}]
```

The baseURL ends in `/v1`: the gateway's upstream path is the baseURL's path plus the format's
suffix. vLLM serves the model under its Hugging Face id first (the llm-d template's
`--served-model-name`, which model-manager repeats after the preset's arguments with the preset
name added: `<id> publishers/<ns>/models/<id> <preset>`, so a request naming the preset also
answers at the model's own route and workload Service), and on agentgateway 2.1 a concrete `AgentgatewayModel` forwards the
request's `model` unchanged — its Custom settings carry no model override, and a transformation of
the field does not reach the provider —, while a virtual model rewrites `model` to its target's.
So the public name is a virtual model over an Internal concrete one matched on the id, which
`GET /v1/models` does not list. The formats are the ones the server registered, so the gateway
converts only what the model does not speak. Both objects stay while their serving object exists
(a model restarting keeps them), are removed by `unload_model` in the same call and when the
serving object is deleted elsewhere, and are rewritten when their spec changes; they are compared
with the served models on every list and at least every five minutes. Two served objects of one
Hugging Face id would share the concrete match, so the router picks the first by name for both.

`list_loaded_models` then reports `publicName` and, as `endpoint`, the endpoint's URL — its public
one (the document's `externalEndpoint`, the chart's `llmRouting.external`) when the installation
publishes it, else the listener's; a served
model that is not on it carries `publicNameReason` (not Ready yet, no interfaces, not created from
a preset, the object could not be written). The model's ModelConfig rides the endpoint: `openAI.
baseUrl` the listener plus `/v1`, `model` the public name, the placeholder key (the in-cluster
listener checks none), so an agent's turns on a local model are metered by the same data plane as
its provider turns. A hand-written `LLMInferenceService` has no preset and stays off the endpoint.
Without the document nothing is written and the ModelConfig keeps the model's own address.

### The ModelConfig's API key

A served model is wired into kagent as a `ModelConfig` on the OpenAI provider — by `load_model`
itself, before the model is ready — and the provider insists on an API key. Which key follows the
model's address: the one KServe published, or, until it does, the one the object is expected on —
its route on the models Gateway when the discovery document names the Gateway
(`ModelServingConfig` `spec.gateway.endpoint`, `https://models.<installation>`; every
`LLMInferenceService` is routed at `<endpoint>/<namespace>/<name>`), else its in-cluster Service:

- **Routed on the models Gateway** (`status.addresses[0].url`, or the expected route — an external
  host such as `https://models.<installation>/model-serving/<name>`): `apiKeyPassthrough: true`,
  no `apiKeySecret`, no Secret. The Gateway's JWT policy admits a person's Dex token and nothing
  else, so the agent forwards the Bearer token of its incoming request as the API key — the
  person reaches the model as themselves. A static or placeholder key answers `401` on every
  turn (`the token header is malformed`).
- **Reached on its in-cluster workload Service** (no Gateway in discovery and no route
  published):
  `apiKeySecret: <name>-api-key` / `apiKeySecretKey: OPENAI_API_KEY`, a placeholder Secret
  model-manager creates and removes with the ModelConfig. vLLM checks no key; the placeholder
  satisfies the runtime.

`wire_model` (`POST /api/v1/models/wire`) takes the shape explicitly for any backend:
`apiKeyPassthrough: true` composes the passthrough shape; `apiKeySecret` (with `apiKeySecretKey`,
default `OPENAI_API_KEY`) references a Secret of the caller's holding a static key for an endpoint
that checks one — never created, never deleted by model-manager. The two together are refused
(`400 invalid`), as the `ModelConfig` CRD refuses them. A re-wire that changes the shape refreshes
the ModelConfig in place and removes a placeholder Secret the new shape no longer reads.
`unwire_model` and `unload_model` remove what was wired, whichever shape.

## Tools

Both tools act as the caller (the request's forwarded token under `--downstream-oauth`, the
ServiceAccount otherwise) and take `dryRun` and `mode`. `mode: apply` writes the ConfigMap directly
and is the only accepted value; `mode: commit` — a pull request through the broker grant — is not
available yet and is refused with that message.

**`add_backend`** — `kind` (required), `name` (default the kind; `kserve-<cluster>` for a further
kserve backend), `source` (default `person`), `endpoint`, `agentEndpoint`,
`credentialsSecret`, `credentialsKey`, and for kserve `cluster`, `organization`, `apiServer`,
`caBundle`, `servingNamespace`, `discoveryNamespace`, `discoveryName`, `gpuPoolTaint`
(`key[=value][:effect]`, kubectl's taint notation) and `gpuPoolNodeSelector`
(`key=value[,key=value]`). With `dryRun: true` it
answers the rendered document and the ConfigMap's namespace, name and labels without writing.
Applied, it creates or replaces the name's ConfigMap (`created: true|false`), waits for the watch
to deliver it (`registered: true`) and returns the backend as `list_backends` reports it. A name
configured statically by the chart values is refused (`conflict`); a document failing the schema is
refused with the field (`invalid_request`).

**`remove_backend`** (destructive) — `name`, the backend's name as `list_backends` shows it (`kind`
is accepted for a backend named after its kind). With `dryRun: true` it lists the ConfigMap
and the ModelConfigs that would go (`modelConfigs`). Applied, it removes model-manager's ModelConfigs of that
backend as the caller (`unwired: [...]`), deletes the ConfigMap (`removed: true`) and waits for the
backend to leave (`deregistered: true`). A ModelConfig of the backend that no model-manager deletes
would outlive the backend pointing at nothing, so the removal is refused (`conflict`, nothing removed)
while one exists. That is one without the `model-manager.giantswarm.io/instance` label (`unwire_model`
removes it once nothing references it), or one Flux applies from git. The refusal names each one by model, ModelConfig and reason, and the dry run lists them as
`blockedBy: [{model, namespace, name, reason}]` with the refusal as `error`. A ModelConfig another
instance created belongs to that instance's backend and is left in place. A static backend cannot be removed here; an absent
document is `not_found`.

**`list_backends`** lists static and registered backends alike, each with `source: static |
person | cluster-manager`, plus `invalid` when documents failed the schema. An empty list carries
`backendsReason`, the `no_backend` wording below, so a caller tells an installation without a
backend from a read that missed one — a ModelConfig labelled with a backend that is not listed is
left over from one that went. `get_info` answers the same beside its `backends`.

## Zero backends and static backends

With neither `backend` nor `backends` set (the chart default) the process starts with no backend:
`list_backends` and `get_info` answer an empty list with `backendsReason` (the wording below), and
every backend-scoped tool (`check_fit`, `load_model`,
`list_models`, …) answers `no_backend` (HTTP 412) naming the instance that answered (its
`--instance` and version), the namespace it watches for backend documents with how many are there
(0 valid, plus the invalid ones `list_backends` lists) and both ways to register one:

```
no_backend: no backend registered on model-manager <instance> <version>: 0 valid backend documents
in namespace <namespace> (ConfigMaps labelled agent-platform.giantswarm.io/model-backend=true);
register one with add_backend kind=kserve servingNamespace=<namespace> (cluster-manager registers
model-backend-kserve-<cluster> with the first GPU node pool it creates on a cluster, one per serving
cluster) or add_backend kind=ollama|lmstudio|lemonade endpoint=<url>, or configure --backends
```

With runtime registration off (`--namespace` empty) the answer says so and names `--backends` as
the only way.

**`check_fit` of a preset without a backend** answers what the preset declares instead, so a person
learns what it needs — and sizes the GPU pool — before the pool, and with it the backend, exists.
The presets are read from the **catalog** the agent-platform connectivity chart publishes where
model-manager runs (the ServingPreset ConfigMaps of the preset namespace: chart value
`kserve.presets.namespace`, else the discovery document's `spec.presets.namespace`, else
model-manager's namespace), with the caller's client first and the ServiceAccount's second, as the
discovery document is read. The answer carries the declaration in the fields a judged answer uses —
`weightsBytes` / `declaredWeightsBytes` (`weightsSource: preset`), `overheadBytes`, `requiredBytes`,
`computeCapabilityRequired` (`requirements.minComputeCapability`), `devicesPerPod` /
`tensorParallel` (the preset's own GPUs), `gpuMemoryUtilization`, `maxModelLen`,
`cacheSource: unknown` — with `verdict: unverified`, `fits: false` (nothing judged the model, and a
load has nothing to compose onto), `retryable: true`, no `backend`, and a `reason` that opens with
the `no_backend` answer above and ends with the declaration: `no_backend: no backend registered on
model-manager <instance> <version>: … or configure --backends — the fit of preset qwen3-8-27b is
unverified until a backend is registered: it declares 1 GPU, 16.0 GiB of weights and 4.0 GiB of
overhead (20.0 GiB required), compute capability 8.9 or newer, --max-model-len 32768`. The preset
is resolved as a backend resolves it: by `preset`, or the single preset serving `model`. An unknown
preset is `not_found` naming the published ones; a model no preset serves (its weights are the hub's
to size, which a backend asks), a cluster with no preset published (the chart renders the catalog
only where model serving is on) and a catalog the caller cannot read stay `no_backend`, naming
why. A registered backend answers as before, the catalog has no say; `backend: ollama` (or any
other name) is never answered from it.

Static values keep working:
`--backends=ollama,lemonade` (chart `backends`) or `--backend=ollama` lists those with `source:
static`, in the operator's order, the first being the default backend; registered backends follow,
sorted by name. A document naming a static kind is refused and reported — the chart values win;
remove the kind there to register it at runtime.

## RBAC

The chart grants the ServiceAccount `get`, `list`, `watch` on ConfigMaps in the release namespace
whenever `rbac.create` is on — also under `oauth.downstream`, since the documents are
model-manager's own configuration, not per-caller data. Writes are the caller's: under
`oauth.downstream` the caller's RBAC governs (cluster-manager and the person need `create`,
`update`, `delete` on the four document names in model-manager's namespace); without it the
ServiceAccount may write exactly `model-backend-{ollama,lmstudio,lemonade,kserve}`.

## The cluster-manager contract

When cluster-manager creates a GPU node pool or enables model serving on `<cluster>` it writes the
`kserve` document above with `source: cluster-manager`, `target.{cluster, organization}` from the
CAPI Cluster, `target.apiServer` and `target.caBundle` from the `<cluster>-kubeconfig` Secret's
*cluster* section (never its user section), `target.servingNamespace` and `discovery.{namespace,
name}` from where its release renders the discovery ConfigMap. It removes the document when the
last pool goes or serving is disabled. Applying the ConfigMap directly and calling `add_backend`
are equivalent — the document is the API; a `ModelBackend` CRD would be the later hardening step
and would keep this shape.
