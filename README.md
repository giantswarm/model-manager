# model-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/model-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/model-manager/tree/main)

Model management service for the Giant Swarm Agent Platform. One API over
per-installation **serving backends**:

| Backend | Where | What it proxies |
|---|---|---|
| `ollama` | laptop / agentlab installs (host Ollama through the kind docker-network gateway) | `/api/tags`, `/api/ps`, streamed `/api/pull`, `/api/delete`, `keep_alive` load/unload |
| `kserve` | GPU installs | InferenceServices, per-node HF cache inventory, download Jobs, serving presets (driver follows behind the same contract) |

The API reports the backend and **explicit capability flags**
(`GET /api/v1/backend`), so clients render only what an installation supports
instead of switching on the backend name. Pulled or loaded models are wired
into kagent automatically as `ModelConfig`s (native keyless `Ollama` provider
for the ollama backend), so agents can use them without manual steps.

The same operations are exposed twice from one process:

- **REST/JSON** under `/api/v1` for the portal backend — contract in
  [`api/openapi.yaml`](api/openapi.yaml), also served at `/api/v1/openapi.yaml`.
- **MCP** (streamable HTTP, `/mcp`) for muster — tools `get_backend`,
  `list_models`, `get_model`, `list_loaded_models`, `pull_model`, `load_model`,
  `unload_model`, `delete_model`, `wire_model`, `unwire_model`, `list_jobs`,
  `get_job`, `cancel_job` (through muster: `x_<server>_<tool>`).

Part of [Model management in the Agent Platform](https://github.com/giantswarm/giantswarm/issues/37590);
decision records: Model Manager PDR and the Ollama-backend ADR in the team's
decision log.

## API at a glance

| Operation | REST | MCP tool |
|---|---|---|
| Backend identity + capabilities | `GET /api/v1/backend` | `get_backend` |
| Downloaded models (with loaded state + ModelConfig) | `GET /api/v1/models`, `GET /api/v1/models/{name}` | `list_models`, `get_model` |
| Loaded / running models | `GET /api/v1/loaded` | `list_loaded_models` |
| Pull / import (returns a job) | `POST /api/v1/models/pull {"model","wire?"}` | `pull_model` |
| Job progress | `GET /api/v1/jobs`, `GET /api/v1/jobs/{id}`, `DELETE /api/v1/jobs/{id}` | `list_jobs`, `get_job`, `cancel_job` |
| Load / unload | `POST /api/v1/models/load {"model","keepAlive?"}`, `POST /api/v1/models/unload` | `load_model`, `unload_model` |
| Delete (unwires by default) | `DELETE /api/v1/models/{name}[?unwire=false]` | `delete_model` |
| Wire / unwire to kagent | `POST /api/v1/models/wire`, `POST /api/v1/models/unwire` | `wire_model`, `unwire_model` |
| Health | `GET /healthz`, `GET /readyz` | — |

Model references may contain `/` and `:` (`smollm2:135m`, `hf.co/org/repo:Q4_K_M`);
path parameters capture the rest of the path. Errors are
`{"error":{"code":"not_found|invalid_request|unsupported|conflict|backend_error","message":"…"}}`;
`unsupported` (501) means the matching capability flag is false.

Capability flags: `pull`, `pullProgress`, `delete`, `load`, `unload`,
`loadedModels`, `wire` (Kubernetes access present), `presets`, `fitCheck`,
`nodeInventory`, `search` (the last four are kserve concerns and false on ollama).

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
`muster.mcpServer.*`. Optional, off by default: `muster.mcpServer.enabled`
(renders an `mcpservers.muster.giantswarm.io` CR), `httpRoute.enabled`,
`networkPolicy.enabled`, `oauth.enabled`.

## Development

See [docs/development.md](docs/development.md).
