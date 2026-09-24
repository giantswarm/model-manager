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
| Name | `model-backend-<kind>` — `model-backend-ollama`, `model-backend-lmstudio`, `model-backend-lemonade`, `model-backend-kserve`; one document per kind |
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

### Schema (enforced when the document is read)

| Field | Required | Meaning |
|---|---|---|
| `apiVersion` | yes | `agent-platform.giantswarm.io/v1alpha1` |
| `kind` | yes | `ModelBackend` |
| `metadata.name` | no | Equals `spec.kind` when set |
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
| `spec.kserve.gpuPool.instances[]` | no | The sizes the pool launches, in the shape cluster-manager's `create_node_pool` answer lists under `sizes`; the fit check judges a model against them while the pool has no node (see below). Every entry: `instanceType` (required, `g6.xlarge`), `size` (`xlarge`; defaults to the part of `instanceType` after the family), `vcpu`, `memoryGiB`, `gpus`, `gpuMemoryGiB` (the memory of one GPU) — positive integers — and `usableVcpu`, `usableMemoryGiB` (positive numbers: what a node of the size leaves a predictor after the kubelet's reservations and the fleet's daemonsets; a `g6.xlarge` 3 / 11.9 of 4 / 16) |

Unknown fields are refused. A document that fails the schema is **reported and not loaded**: it
appears under `invalid` in `list_backends` with the ConfigMap name and the failing field
(`spec.endpoint: required for ollama`), and the process keeps running with the backends it has.
Fixing the ConfigMap loads it; deleting it clears the report.

The target **never carries credentials**: every Kubernetes call the kserve backend makes presents
the caller's own token (`--downstream-oauth`, the platform default), so the target apiserver must
trust the installation's Dex (the cluster chart's OIDC / `structuredAuthentication` values). A call
without a caller — no token in the request — is anonymous there and refused. A `local` target uses
model-manager's in-cluster address and CA.

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
`budgetSource: pool-scale-from-zero` and a reason that says the fit is unverified; Karpenter is
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
  memory alone.

The document is what tells the fit there is a pool at all. Settings resolved while the discovery
ConfigMap was not published yet — the serving slice publishes it as its connectivity child installs —
stand for five seconds, not the minute the document's settings stand, and a fit that finds no
candidate node while its settings lack the document re-reads it once before answering: with a pool
named, the scale-from-zero verdict above; still without it, `fits: false`, `retryable: true` and
`reason: the serving layer's discovery document <namespace>/<name> is not published yet — the slice
is still installing; retry in a moment`, which `load_model` / `pull_model` echo in their refusal.
`no accelerator node` is the verdict for a document that names no pool.

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

A server that publishes no route list (a runtime started with `--disable-fastapi-docs`, which the
agent-platform chart refuses in a preset), a vLLM before 0.16 (it registers every route whatever
the model serves) or a server that could not be read reports `interfaces: []` and
`interfacesReason`; nothing is inferred from a version. A read that got no answer — the serving
namespace's network policy dropping model-manager's connection, a runtime failing — is repeated
after a minute; an answer stands until the model turns Ready anew.

Whether a registered interface serves a given request is the preset's business: tool calling
needs `--enable-auto-tool-choice` and a `--tool-call-parser`, thinking blocks a
`--reasoning-parser`.

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

**`add_backend`** — `kind` (required), `source` (default `person`), `endpoint`, `agentEndpoint`,
`credentialsSecret`, `credentialsKey`, and for kserve `cluster`, `organization`, `apiServer`,
`caBundle`, `servingNamespace`, `discoveryNamespace`, `discoveryName`, `gpuPoolTaint`
(`key[=value][:effect]`, kubectl's taint notation) and `gpuPoolNodeSelector`
(`key=value[,key=value]`). With `dryRun: true` it
answers the rendered document and the ConfigMap's namespace, name and labels without writing.
Applied, it creates or replaces the kind's ConfigMap (`created: true|false`), waits for the watch
to deliver it (`registered: true`) and returns the backend as `list_backends` reports it. A kind
configured statically by the chart values is refused (`conflict`); a document failing the schema is
refused with the field (`invalid_request`).

**`remove_backend`** (destructive) — `kind` (required). With `dryRun: true` it lists the ConfigMap
and the ModelConfigs that would go. Applied, it removes model-manager's ModelConfigs of that
backend as the caller (`unwired: [...]`), deletes the ConfigMap (`removed: true`) and waits for the
backend to leave (`deregistered: true`). A static backend cannot be removed here; an absent
document is `not_found`.

**`list_backends`** lists static and registered backends alike, each with `source: static |
person | cluster-manager`, plus `invalid` when documents failed the schema.

## Zero backends and static backends

With neither `backend` nor `backends` set (the chart default) the process starts with no backend:
`list_backends` answers an empty list and every backend-scoped tool answers
`no_backend: no backend registered: register one with add_backend (kind
ollama|lmstudio|lemonade|kserve) or configure --backends` (HTTP 412). Static values keep working:
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
