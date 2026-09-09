# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `agent-platform.giantswarm.io/tool-group: agent-platform` on the muster `MCPServer` CR, next to `muster.giantswarm.io/type`: model-manager is part of the Agent Platform's own management surface, the tier the portal's MCP servers page, the toolset presets and the docs group it under. `muster.mcpServer.labels` still adds to and overrides the CR's labels; the passthrough is merged instead of appended, so an override renders one key instead of a duplicate.
- `lmstudio` backend: an LM Studio on the host (llama.cpp on GPU and CPU, MLX on Apple silicon) as a serving backend, proxying its `/api/v1` API — the library, the download that backs a pull with real byte progress, and load/unload. Agent wiring uses kagent's `OpenAI` provider against the agent host plus `/v1` with the placeholder key. LM Studio serves no delete (`lms rm` is host-only), so the backend reports `delete: false` and the call answers `501 unsupported`; it exposes no host hardware, so `nodeInventory` is false too. A load is idempotent (LM Studio loads an *instance*, so a repeat would pin a second copy), load and unload prove themselves from the answer's body rather than its status code, a pull resolves its reference against the library before reporting success (LM Studio does not promise the download's `key` is the reference asked for) and gives up on a status that is neither progress nor an end it knows. Chart values `lmstudio.endpoint` / `lmstudio.agentHost`, flags `--lmstudio-endpoint` / `--lmstudio-agent-host`. Requires LM Studio 0.4.0+.

### Changed

- Agent wiring targets kagent API v2: the ModelConfig API version used when discovery is unavailable is `kagent.dev/v1alpha3`, the only version kagent API v2 serves (`kagent.apiVersion: auto` keeps resolving whatever the cluster serves). The spec model-manager writes — `provider`, `model`, `ollama.host`, `openAI.baseUrl`, `apiKeySecret`/`apiKeySecretKey` — is the same in v1alpha2 and v1alpha3. A ModelConfig's `ready` now needs kagent's `ResolvedRefs` condition beside `Accepted`: kagent API v2 reports a missing or incomplete API-key Secret there while `Accepted` stays `True`, so such a ModelConfig read as ready before; `message` names the condition holding it back.

### Fixed

- The `helm.sh/chart` label value is valid for every chart version: the 63-character cut of a branch build's version (`0.x.y-dev.<branch>.<timestamp>.<sha>`) could end on a `.`, which the API server rejects on every labelled object (the ATS smoke of a 16-character branch name failed that way). The helper trims a trailing `.` like it trims a trailing `-`.


[Unreleased]: https://github.com/giantswarm/model-manager/tree/main
