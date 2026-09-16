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
| `spec.kserve.target.servingNamespace` | yes | Namespace on the target holding the InferenceServices, download Jobs and the cache |
| `spec.kserve.discovery.namespace` | no | Where the model-serving discovery ConfigMap (`kind: ModelServingConfig`) lives on the target; defaults to `servingNamespace` |
| `spec.kserve.discovery.name` | no | Its name; defaults to `agent-platform-model-serving` |
| `spec.kserve.gpuPool` | no | The GPU node pool's scheduling; replaces the discovery ConfigMap's `spec.gpuPool` (see below) |
| `spec.kserve.gpuPool.taint.{key,value,effect}` | no | The pool's taint (`key` required; `effect` `NoSchedule` \| `PreferNoSchedule` \| `NoExecute`, empty for every effect). Tolerated by the inventory scan pods, the download Jobs and the predictors model-manager composes: `value` empty tolerates every value (`Exists`), set compares it (`Equal`) |
| `spec.kserve.gpuPool.nodeSelector` | no | The pool's label(s) (`giantswarm.io/machine-pool: <cluster>-<pool>`), the node selector of the composed predictors and of every scan pod and download Job that is not pinned to a node |
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
| Inventory scan pod (`kserve.inventory.mode: pod`) | always | likewise; the chart's DaemonSet (`daemonset` mode) takes `kserve.inventory.agent.tolerations` / `.nodeSelector` |

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
  against the size's, and the weights plus overhead the check sized against `gpus × gpuMemoryGiB`;
- the **smallest size that hosts it** is the node it will come as: `fits: true`, `instanceType`
  names the size (`g6.xlarge`), `budgetBytes` is its GPU memory, and the reason says the size's
  leftovers;
- when **no size hosts it**: `fits: false`, the reason names the pool's sizes, what the predictor
  asks and what the largest size leaves it — and `load_model` / `pull_model` refuse with that
  reason before any object is created, instead of a predictor that can only sit Pending;
- a model **without a preset** is judged on its weights and the default overhead against the GPU
  memory alone.

A list with an invalid entry is refused on the document (the document is reported and not loaded)
and ignored from discovery (the answer is the unverified one). Once a node of the pool exists the
fit is against that node again, as before.

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
