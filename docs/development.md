# Developing on model-manager

```sh
make build          # binary for the current platform
go test ./...       # unit tests (httptest fake Ollama and Lemonade, fake dynamic kube client)
make lint           # golangci-lint with the pre-commit linters (gosec, goconst, govet)
make helm-schema    # regenerate helm/model-manager/values.schema.json
make helm-docs      # regenerate helm/model-manager/README.md
```

## Layout

- `cmd/` — cobra CLI (`serve`, `version`).
- `internal/backend` — the `Backend` interface, capability flags, shared types
  and the driver registry, plus the optional interfaces a driver may implement
  (`PresetLister`, `Searcher`, `FitChecker`, `NodeLister`, `ServeLifecycle`,
  `PullAdopter`). `internal/backend/ollama` is the host-Ollama driver;
  `internal/backend/lemonade` the Lemonade Server driver (`client.go` — the
  management API including the SSE pull, `backend.go`, `nodes.go` — the host
  from system-info). `internal/backend/kserve` is the KServe driver: `config.go` (discovery
  ConfigMap + flag overrides), `presets.go`, `hub.go` (Hugging Face Hub),
  `nodes.go` (budgets, cache location), `inventory.go` (cache scan pods and
  the cache-agent client), `internal/cacheagent` (the DaemonSet's HTTP
  inventory, `model-manager cache-agent`),
  `jobs.go` (download Jobs), `isvc.go` (InferenceService composition/status),
  `fit.go`, `backend.go`.
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

### kserve backend in the lab (no controller)

The lab has the KServe CRDs but no controller and no GPUs, which is enough for
everything except a real vLLM start. Install a second release in its own
namespace with the modelServing ConfigMaps rendered from
`agent-platform-standalone` and a local-path cache claim.

Render the ConfigMaps from the umbrella chart with the lab's own values file
(`state/agent-platform-values.yaml` in the agentlab checkout): a defaults-only
render fails on the umbrella's valkey/gateway validation guards, and
`components.modelServing.namespace.name` moves the ConfigMaps into the release
namespace. Use chart 0.14.0 or newer; that is where the serving-namespace
network policies and the discovery `networkPolicy` field arrive. `yq` below is
the Python jq wrapper (`-y` for YAML output); with the Go yq drop `-y`:

```sh
kubectl create ns mm-kserve
helm pull oci://gsoci.azurecr.io/charts/giantswarm/agent-platform-standalone --version 0.14.0 --untar
helm template lab ./agent-platform-standalone -n mm-kserve \
  -f ~/projects/giantswarm/agentlab/state/agent-platform-values.yaml \
  --set components.modelServing.enabled=true --set components.modelServing.namespace.name=mm-kserve \
  --api-versions serving.kserve.io/v1alpha1 --api-versions serving.kserve.io/v1beta1 \
  | yq -y 'select(.kind == "ConfigMap" and .metadata.labels."app.kubernetes.io/component" == "model-serving")' \
  | kubectl -n mm-kserve apply -f -
kubectl -n mm-kserve create -f - <<PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: hf-cache}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 5Gi}}}
PVC
helm upgrade --install model-manager-kserve helm/model-manager -n mm-kserve \
  --set backend=kserve --set kserve.namespace=mm-kserve --set fullnameOverride=model-manager-kserve \
  --set image.registry=docker.io --set image.repository=library/model-manager \
  --set image.tag=dev-$(git rev-parse --short HEAD) --set image.pullPolicy=Never \
  --set muster.mcpServer.enabled=true --set muster.mcpServer.name=model-manager-kserve
```

The chart's cluster-scoped RBAC (`ClusterRole`/`ClusterRoleBinding`
`<fullname>-nodes`) is named after the release, so a leftover `<release>-nodes`
from a release someone else has already deleted makes `helm install` refuse the
same release name (`invalid ownership metadata`). Pick a new release name and
pass it as `fullnameOverride` so the Service, DaemonSet and RBAC names follow
it; never delete other people's objects to free the name.

`GET /api/v1/search?q=tiny-random-gpt2`, `POST /api/v1/models/fit-check`,
`POST /api/v1/models/pull` (a Job downloads into the claim), `GET /api/v1/models`
(the cache entry with size and node), `POST /api/v1/models/load` (an
InferenceService appears; patch its status Ready with
`kubectl patch --subresource=status` to see the load job wire a ModelConfig),
`POST /api/v1/models/unload`, `DELETE /api/v1/models/<repo>`.

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
