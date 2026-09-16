# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Fixed

- kserve: a GPU pool at scale-to-zero can be served (giantswarm/model-manager#90). When the pool selector (`spec.gpuPool.nodeSelector`) names a pool no node belongs to yet, `check_fit` answers `fits: true` without a node (`budgetSource: pool-scale-from-zero`, the reason says the fit is unverified) and `load_model` / `pull_model` proceed: the predictor carries the pool's toleration and selector, goes Pending, and the autoscaler launches the node. Before, `no eligible node` refused every load on an empty pool, so the first model could never be served on it. An explicit node, a pool whose nodes do not fit, or no pool selector keep the refusal.

### Added

- kserve: the GPU node pool's scheduling as one input, `gpuPool` — the pool's taint (`nvidia.com/gpu` `NoSchedule`, the taint a platform-created pool carries) tolerated and the pool's label (`giantswarm.io/machine-pool=<cluster>-<pool>`) selected on everything the driver schedules onto the pool: the composed `LLMInferenceService` / `InferenceService` predictors (the pool toleration first, the preset's `scheduling.tolerations` after it), the download Jobs and the inventory scan pods (the toleration always, the selector unless the pod is pinned to a cache node). Read from the discovery ConfigMap's `spec.gpuPool.taint` / `spec.gpuPool.nodeSelector` (the platform chart's `modelServing.gpuPool.*`); a registered backend document's `spec.kserve.gpuPool` (and `add_backend`'s `gpuPoolTaint` / `gpuPoolNodeSelector`) replaces it. `GET /api/v1/nodes` reports a tainted GPU node as capacity once its taint is tolerated and names an untolerated `NoSchedule`/`NoExecute` taint or a node outside the pool's selector in `eligibilityReason`; `GET /api/v1/backend` reports the input as `gpuPool`. Unset, nothing changes.
- kserve: a serving preset is composed into an `LLMInferenceService` (`serving.kserve.io/v1alpha2`, the llm-d control plane) by spec shape wherever that API is served — `spec.model.uri`/`name`, one replica, `router.route` and `router.scheduler`, the preset's args, env and resources on `template.containers[main]`, `scheduling` as the template's nodeSelector/tolerations, the chat template mounted, `template.runtimeClassName` from the discovery ConfigMap when set; no `baseRefs` and no image unless the preset's new `spec.template` (containers merged by name) or `spec.baseRefs` say so. `--kserve-serving-kind` / `kserve.servingKind` (`auto` | `LLMInferenceService` | `InferenceService`) pins the kind; both kinds are listed, stopped and deleted, `LoadedModel.kind` names it, and the wiring's served model name follows the kind. Composer tests validate every shipped preset against the LLMInferenceService CRD schema.
- `agent-platform.giantswarm.io/tool-group: agent-platform` on the muster `MCPServer` CR, next to `muster.giantswarm.io/type`: model-manager is part of the Agent Platform's own management surface, the tier the portal's MCP servers page, the toolset presets and the docs group it under. `muster.mcpServer.labels` still adds to and overrides the CR's labels; the passthrough is merged instead of appended, so an override renders one key instead of a duplicate.
- `lmstudio` backend: an LM Studio on the host (llama.cpp on GPU and CPU, MLX on Apple silicon) as a serving backend, proxying its `/api/v1` API — the library, the download that backs a pull with real byte progress, and load/unload. Agent wiring uses kagent's `OpenAI` provider against the agent host plus `/v1` with the placeholder key. LM Studio serves no delete (`lms rm` is host-only), so the backend reports `delete: false` and the call answers `501 unsupported`; it exposes no host hardware, so `nodeInventory` is false too. A load is idempotent (LM Studio loads an *instance*, so a repeat would pin a second copy), load and unload prove themselves from the answer's body rather than its status code, a pull resolves its reference against the library before reporting success (LM Studio does not promise the download's `key` is the reference asked for) and gives up on a status that is neither progress nor an end it knows. Chart values `lmstudio.endpoint` / `lmstudio.agentHost`, flags `--lmstudio-endpoint` / `--lmstudio-agent-host`. Requires LM Studio 0.4.0+.

### Changed

- Agent wiring targets kagent API v2: the ModelConfig API version used when discovery is unavailable is `kagent.dev/v1alpha3`, the only version kagent API v2 serves; `kagent.apiVersion: auto` keeps resolving whatever the cluster serves, so a kagent 0.x cluster still gets `v1alpha2`. The spec model-manager writes — `provider`, `model`, `ollama.host`, `openAI.baseUrl`, `apiKeySecret`/`apiKeySecretKey` — is the same in both versions. A ModelConfig's `ready` needs kagent's `ResolvedRefs` condition to be `True` beside `Accepted` whenever the controller reports it: kagent API v2 reports a missing or incomplete API-key Secret there while `Accepted` stays `True`, so such a ModelConfig read as ready before; `message` names the condition holding it back. The chart-test smoke applies the v2 `modelconfigs` CRD.

- The chart README no longer renders a version badge (`chart.badgesSection` removed from `README.md.gotmpl`): a release PR bumping `Chart.yaml`'s `version` no longer changes the checked-in `README.md`, so the helm-docs pre-commit hook no longer fails on it. ([giantswarm/devctl#2180](https://github.com/giantswarm/devctl/issues/2180))

### Fixed

- The `helm.sh/chart` label value is valid for every chart version: the 63-character cut of a branch build's version (`0.x.y-dev.<branch>.<timestamp>.<sha>`) could end on a `.`, which the API server rejects on every labelled object (the ATS smoke of a 16-character branch name failed that way). The helper trims a trailing `.` like it trims a trailing `-`.


[Unreleased]: https://github.com/giantswarm/model-manager/tree/main
