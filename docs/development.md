# Developing on model-manager

```sh
make build          # binary for the current platform
go test ./...       # unit tests (httptest fake Ollama, Lemonade and LM Studio, fake dynamic kube client)
make lint           # golangci-lint with the pre-commit linters (gosec, goconst, govet)
make helm-schema    # regenerate helm/model-manager/values.schema.json
make helm-docs      # regenerate helm/model-manager/README.md
```

## Layout

- `cmd/` — cobra CLI (`serve`, `cache-agent`, `version`). The version is the tag at HEAD when the binary is built from a tagged checkout (`internal/buildinfo`, from the Go build info; no `-ldflags -X` needed), `dev` for a build without version control information.
- `internal/backend` — the `Backend` interface, capability flags, shared types
  and the driver registry, plus the optional interfaces a driver may implement
  (`PresetLister`, `Searcher`, `FitChecker`, `NodeLister`, `ServeLifecycle`,
  `PullAdopter`). `internal/backend/ollama` is the host-Ollama driver;
  `internal/backend/lemonade` the Lemonade Server driver (`client.go` — the
  management API including the SSE pull, `backend.go`, `nodes.go` — the host
  from system-info); `internal/backend/lmstudio` the LM Studio driver
  (`client.go` — the /api/v1 API, `backend.go`; no `nodes.go`, LM Studio
  exposes no host hardware, and no delete). `internal/backend/kserve` is the KServe driver: `config.go` (discovery
  ConfigMap + flag overrides), `presets.go`, `hub.go` (Hugging Face Hub),
  `nodes.go` (budgets, cache location), `inventory.go` (cache scan Jobs and
  the cache-agent client), `scangate.go` (when a scan may run: never on an
  unbound claim or a pool at zero, never inline under a short deadline), `internal/cacheagent` (the DaemonSet's HTTP
  inventory, `model-manager cache-agent`),
  `jobs.go` (download Jobs), `llmisvc.go` (LLMInferenceService composition),
  `served.go` (the served objects: listing, status, addresses), `fit.go`,
  `backend.go`.
- `internal/jobs` — in-memory job manager (pulls with progress, cancel, retention).
- `internal/wiring` — kagent `ModelConfig` create/update/delete via the dynamic
  client; owns only CRs labelled `app.kubernetes.io/managed-by=model-manager`.
- `internal/service` — orchestration shared by both API surfaces.
- `internal/api` — REST handlers and MCP tools over the service.
- `internal/server` — HTTP listener; with `--enable-oauth` the mcp-oauth
  resource server (Dex or Google) in front of REST and MCP.
- `internal/identity` — the authenticated caller on the request context and,
  with `--downstream-oauth`, the caller's IdP token the Kubernetes clients use.
- `internal/kube` — Kubernetes clients: the ServiceAccount's, and per-caller
  ones built from the caller's token.
- `api/openapi.yaml` — the REST contract; served at `/api/v1/openapi.yaml`.
- `helm/model-manager` — the chart.

## Local loop against a host Ollama and a Lemonade Server at once

```sh
./model-manager serve --listen 127.0.0.1:18080 --backends ollama,lemonade \
  --ollama-endpoint http://localhost:11434 --ollama-agent-host http://172.21.0.1:11434 \
  --lemonade-endpoint http://localhost:13305 --lemonade-agent-host http://172.21.0.1:13305 \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab --kagent-namespace kagent -v

curl -s localhost:18080/api/v1/backends                       # both descriptors, ollama first (the default)
curl -s localhost:18080/api/v1/models                         # every model, each with "backend"
curl -s -X POST localhost:18080/api/v1/models/pull -d '{"model":"qwen3-4b-FLM","backend":"lemonade"}'
curl -s -X POST localhost:18080/api/v1/models/load -d '{"model":"qwen3-4b-FLM","keepAlive":"-1"}'   # unique: no backend needed
curl -s 'localhost:18080/api/v1/models/qwen3:0.6b?backend=ollama'
```

## Local loop against a host Ollama and the agentlab kind cluster

```sh
make build
./model-manager serve --listen 127.0.0.1:18080 \
  --ollama-endpoint http://localhost:11434 --ollama-agent-host http://172.21.0.1:11434 \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab --kagent-namespace kagent -v

curl -s localhost:18080/api/v1/backend
curl -s -X POST localhost:18080/api/v1/models/pull -d '{"model":"smollm2:135m"}'
curl -s localhost:18080/api/v1/jobs/<id>
kubectl -n kagent get modelconfigs.kagent.dev smollm2-135m
curl -s -X POST localhost:18080/api/v1/models/load -d '{"model":"smollm2:135m","keepAlive":"10m"}'
curl -s -X POST localhost:18080/api/v1/models/unload -d '{"model":"smollm2:135m"}'
curl -s -X DELETE localhost:18080/api/v1/models/smollm2:135m
```

## Local loop against a Lemonade Server (AMD Ryzen AI NPU)

Lemonade listens on 13305 by default; the `*-FLM` models are the NPU ones.

```sh
./model-manager serve --listen 127.0.0.1:18080 --backend lemonade \
  --lemonade-endpoint http://localhost:13305 --lemonade-agent-host http://172.21.0.1:13305 \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab --kagent-namespace kagent -v

curl -s localhost:18080/api/v1/backend            # lemonade, version, agentEndpoint …/api/v1, loading
curl -s localhost:18080/api/v1/models             # runtime: flm / llamacpp, mapped capabilities
curl -s -X POST localhost:18080/api/v1/models/pull -d '{"model":"Qwen3-0.6B-GGUF"}'
curl -s -X POST localhost:18080/api/v1/models/load -d '{"model":"qwen3-it-4b-FLM","keepAlive":"-1"}'
curl -s localhost:18080/api/v1/loaded             # device: npu, pinned: true
curl -s localhost:18080/api/v1/nodes              # budgetSource: system-info, gpuProduct: the NPU, cache: the model store
curl -s -X POST localhost:18080/api/v1/models/unload -d '{"model":"qwen3-it-4b-FLM"}'
```

In the lab, install the chart with `--set backend=lemonade --set
lemonade.endpoint=http://172.21.0.1:13305` (the kind docker network gateway;
Lemonade bound to `0.0.0.0`, port 13305 open to the bridge subnets) next to
the umbrella's release, as in the ollama recipe below.

## Local loop against an LM Studio

LM Studio listens on 1234 by default and needs 0.4.0 or newer (the `/api/v1`
API). Start it with `lms server start --bind 0.0.0.0`, or the app's Developer
→ "Serve on Local Network" toggle.

```sh
./model-manager serve --listen 127.0.0.1:18080 --backend lmstudio \
  --lmstudio-endpoint http://localhost:1234 --lmstudio-agent-host http://172.21.0.1:1234 \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab --kagent-namespace kagent -v

curl -s localhost:18080/api/v1/backend            # lmstudio, agentEndpoint …/v1, delete: false, no version
curl -s localhost:18080/api/v1/models             # the library; capabilities from trained_for_tool_use, vision, reasoning
curl -s -X POST localhost:18080/api/v1/models/pull -d '{"model":"ibm/granite-4-micro"}'
curl -s -X POST localhost:18080/api/v1/models/load -d '{"model":"ibm/granite-4-micro"}'
curl -s localhost:18080/api/v1/loaded             # the loaded instances
curl -s -X POST localhost:18080/api/v1/models/unload -d '{"model":"ibm/granite-4-micro"}'
curl -s -X DELETE localhost:18080/api/v1/models/ibm/granite-4-micro   # 501 unsupported: use `lms rm` on the host
```

Two things to expect while developing against it. LM Studio answers **HTTP 200
with an `{"error": …}` body for every path outside `/api/v1`** (including
Ollama's `/api/version`), so a probe must check the shape of the answer, never
its status code. And the model key carries a slash — the REST routes take it
raw (`{name...}`), so `GET /api/v1/models/ibm/granite-4-micro` is the shape,
not a percent-encoded one.

In the lab, install the chart with `--set backend=lmstudio --set
lmstudio.endpoint=http://172.21.0.1:1234`.

## In the lab (agentlab)

```sh
make docker-build TAG=model-manager:dev-$(git rev-parse --short HEAD)
kind load docker-image model-manager:dev-$(git rev-parse --short HEAD) --name agentlab
helm upgrade --install model-manager helm/model-manager -n agent-platform \
  --set image.registry=docker.io --set image.repository=library/model-manager \
  --set image.tag=dev-$(git rev-parse --short HEAD) --set image.pullPolicy=Never \
  --set ollama.endpoint=http://172.21.0.1:11434 --set muster.mcpServer.enabled=true
```

Then exercise the REST API through a port-forward and the MCP tools through
muster (`x_model-manager_*`).

### kserve backend in the lab

The lab's serving switch installs the llm-d control plane (KServe's
`kserve-llmisvc-crd`, `kserve-llmisvc-resources` and `kserve-runtime-configs`
components) and the connectivity chart's serving slice — the discovery
ConfigMap `agent-platform-model-serving`, the presets and the models Gateway
in the `agent-platform` namespace; the kind node has no GPU, so the shipped
GPU presets are refused by the fit check and the lab publishes a CPU preset
(`resources.gpus: 0`, the llm-d CPU runtime image) that serves on the node.
Install a second release in its own namespace, reading the platform's
discovery document and presets, with its own serving namespace and a
local-path cache claim:

```sh
kubectl create ns mm-dev
kubectl -n mm-dev create -f - <<PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: hf-cache}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 2Gi}}}
PVC
helm upgrade --install mm-dev helm/model-manager -n mm-dev \
  --set backend=kserve --set kserve.namespace=mm-dev --set kserve.discovery.namespace=agent-platform \
  --set fullnameOverride=mm-dev \
  --set image.registry=docker.io --set image.repository=library/model-manager \
  --set image.tag=dev-$(git rev-parse --short HEAD) --set image.pullPolicy=Never
```

The chart's cluster-scoped RBAC (`ClusterRole`/`ClusterRoleBinding`
`<fullname>-nodes`) is named after the release, so a leftover `<release>-nodes`
from a release someone else has already deleted makes `helm install` refuse the
same release name (`invalid ownership metadata`). Pick a new release name and
pass it as `fullnameOverride` so the Service, DaemonSet and RBAC names follow
it; never delete other people's objects to free the name.

`GET /api/v1/backends` (healthy, and `message` empty: the API is served and
the well-known `LLMInferenceServiceConfig` found — on a cluster with the CRDs
alone it names the missing controller and `POST /api/v1/models/load` answers
`503 unavailable` without creating anything), `GET /api/v1/presets`,
`POST /api/v1/models/fit-check` (the CPU preset fits the kind node against
its allocatable memory, a GPU preset is refused with `no accelerator node`),
`POST /api/v1/models/load` (an `LLMInferenceService` appears in the release's
serving namespace and the llm-d controller reconciles it — the workload
Deployment, the `HTTPRoute` on the models Gateway, the status conditions —
and the load job wires a ModelConfig once it is Ready), `GET /api/v1/loaded`,
`POST /api/v1/models/unload`.

The DaemonSet inventory (`kserve.inventory.mode=daemonset`) is exercised the
same way — the kind node is the cache node, so the DaemonSet needs no node
selector there:

```sh
helm upgrade --install model-manager-kserve helm/model-manager -n mm-kserve --reuse-values \
  --set kserve.inventory.mode=daemonset --set kserve.cache.claimName=hf-cache
kubectl -n mm-kserve rollout status ds/model-manager-kserve-cache-agent
```

`GET /api/v1/nodes` then reports `cache.inventory: daemonset` with the same
entries as before, and no `mm-scan-*` pods appear in the namespace.
