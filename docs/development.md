# Developing on model-manager

```sh
make build          # binary for the current platform
go test ./...       # unit tests (httptest fake Ollama, fake dynamic kube client)
make lint           # golangci-lint with the pre-commit linters (gosec, goconst, govet)
make helm-schema    # regenerate helm/model-manager/values.schema.json
make helm-docs      # regenerate helm/model-manager/README.md
```

## Layout

- `cmd/` — cobra CLI (`serve`, `version`).
- `internal/backend` — the `Backend` interface, capability flags, shared types
  and the driver registry, plus the optional interfaces a driver may implement
  (`PresetLister`, `Searcher`, `FitChecker`, `NodeLister`, `ServeLifecycle`,
  `PullAdopter`). `internal/backend/ollama` is the host-Ollama driver.
  `internal/backend/kserve` is the KServe driver: `config.go` (discovery
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
- `internal/server` — HTTP listener, optional OAuth (mcp-oauth, Dex).
- `api/openapi.yaml` — the REST contract; served at `/api/v1/openapi.yaml`.
- `helm/model-manager` — the chart.

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
`agent-platform-standalone` and a local-path cache claim:

```sh
kubectl create ns mm-kserve
helm template lab oci://gsoci.azurecr.io/charts/giantswarm/agent-platform-standalone --version 0.10.0 \
  -n mm-kserve --set components.modelServing.enabled=true \
  --api-versions serving.kserve.io/v1alpha1 --api-versions serving.kserve.io/v1beta1 \
  | yq 'select(.kind == "ConfigMap")' | kubectl -n mm-kserve apply -f -
kubectl -n mm-kserve create -f - <<PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: hf-cache}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 5Gi}}}
PVC
helm upgrade --install model-manager-kserve helm/model-manager -n mm-kserve \
  --set backend=kserve --set kserve.namespace=mm-kserve \
  --set image.registry=docker.io --set image.repository=library/model-manager \
  --set image.tag=dev-$(git rev-parse --short HEAD) --set image.pullPolicy=Never \
  --set muster.mcpServer.enabled=true --set muster.mcpServer.name=model-manager-kserve
```

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
