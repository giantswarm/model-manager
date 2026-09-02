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
  and the driver registry. `internal/backend/ollama` is the host-Ollama driver.
  A kserve driver implements the same interface (`ListModels` = per-node HF
  cache, `Pull` = download Job, `Load`/`Unload` = InferenceService with
  `LoadRequest.Preset`) and registers itself in `cmd/serve.go`.
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
