# The tools

Generated from the registered tools (`make tools-doc`); through muster every tool is `x_<server>_<name>`. Every configuration registers all of them; `add_backend` and `remove_backend` refuse without Kubernetes access.

| Tool | Kind |
|---|---|
| `add_backend` | write, destructive |
| `cancel_job` | write, destructive |
| `check_fit` | read-only |
| `delete_model` | write, destructive |
| `get_backend` | read-only |
| `get_info` | read-only |
| `get_job` | read-only |
| `get_model` | read-only |
| `list_backends` | read-only |
| `list_jobs` | read-only |
| `list_loaded_models` | write |
| `list_models` | read-only |
| `list_nodes` | read-only |
| `list_presets` | read-only |
| `load_model` | write |
| `pull_model` | write |
| `remove_backend` | write, destructive |
| `search_models` | read-only |
| `unload_model` | write, destructive |
| `unwire_model` | write, destructive |
| `wire_model` | write, destructive |

## `add_backend`

```text
Register a serving backend at runtime by writing its backend document, a ConfigMap in model-manager's namespace, as you.

One backend per name (default: the kind); kserve takes one per serving cluster, named kserve-<cluster> beside the installation's own kserve. An existing document of the name is replaced; a name the chart configures is refused. Host backends (ollama, lmstudio, lemonade) take endpoint; kserve takes the target cluster (local, else apiServer and caBundle, never credentials) and servingNamespace. mode commit is not available yet.
```

```json
{
  "properties": {
    "agentEndpoint": {
      "description": "Host backend base URL as reached by agent pods (default: endpoint)",
      "type": "string"
    },
    "apiServer": {
      "description": "kserve: the target apiserver URL (required unless cluster is local)",
      "type": "string"
    },
    "caBundle": {
      "description": "kserve: the target apiserver's CA bundle, PEM (required unless cluster is local)",
      "type": "string"
    },
    "cluster": {
      "description": "kserve: target cluster name; local (default) is the cluster model-manager runs on",
      "type": "string"
    },
    "credentialsKey": {
      "description": "Key in that Secret (default token)",
      "type": "string"
    },
    "credentialsSecret": {
      "description": "kserve: Secret in the serving namespace holding the Hugging Face token (optional)",
      "type": "string"
    },
    "discoveryName": {
      "description": "kserve: name of the discovery ConfigMap (default agent-platform-model-serving)",
      "type": "string"
    },
    "discoveryNamespace": {
      "description": "kserve: namespace of the model-serving discovery ConfigMap (default: the serving namespace)",
      "type": "string"
    },
    "dryRun": {
      "description": "Render the document and the ConfigMap without writing (default false)",
      "type": "boolean"
    },
    "endpoint": {
      "description": "Host backend base URL as reached by model-manager (ollama, lmstudio, lemonade)",
      "type": "string"
    },
    "gpuPoolNodeSelector": {
      "description": "kserve: the GPU pool's node labels, key=value[,key=value] (giantswarm.io/machine-pool=<cluster>-<pool>); replaces the discovery's gpuPool.nodeSelector",
      "type": "string"
    },
    "gpuPoolTaint": {
      "description": "kserve: the GPU pool's taint, key[=value][:effect] (nvidia.com/gpu:NoSchedule); replaces the discovery ConfigMap's gpuPool.taint",
      "type": "string"
    },
    "kind": {
      "description": "Backend kind: ollama|lmstudio|lemonade|kserve",
      "type": "string"
    },
    "mode": {
      "description": "apply (default): write the ConfigMap as you. commit is not available yet",
      "type": "string"
    },
    "name": {
      "description": "Backend name (default: the kind); kserve also takes kserve-<cluster>, one backend per serving cluster",
      "type": "string"
    },
    "organization": {
      "description": "kserve: the target cluster's organization",
      "type": "string"
    },
    "servingNamespace": {
      "description": "kserve: the serving namespace on the target (required)",
      "type": "string"
    },
    "source": {
      "description": "Who registers it: person (default) or cluster-manager",
      "type": "string"
    }
  },
  "required": [
    "kind"
  ],
  "type": "object"
}
```

## `cancel_job`

```text
Cancel a running pull or load job; cancelling a load leaves the model serving (unload_model stops it).
```

```json
{
  "properties": {
    "dryRun": {
      "description": "Write nothing; return the plan: the job it would cancel",
      "type": "boolean"
    },
    "id": {
      "description": "Job id",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `check_fit`

```text
Check whether a model or serving preset fits a kserve node before pulling or loading: the verdict load_model reaches.

Sizes the weights from the hub (an oci:// preset from its image), adds the serving overhead and vLLM's KV cache at --max-model-len, and compares with the node's free budget less what served models reserve. fits stands only on the checks made: skippedChecks names those it could not make, reason any estimate. A GPU pool with no ready node is judged on its instance shapes (unverified without them). Says whether the model is cached there and needs a hub token.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to check on; required when several backends offer fit checks, as list_backends names it; optional with one backend",
      "type": "string"
    },
    "model": {
      "description": "Hugging Face repository owner/name (required unless preset is given)",
      "type": "string"
    },
    "node": {
      "description": "Node to check against; default: the best eligible node",
      "type": "string"
    },
    "nodes": {
      "description": "kserve: split, the nodes to split across (default: the first fast link hosting the model); copies, one copy on each, fitting when every copy does",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "placement": {
      "description": "kserve: copies (default; each holds the whole model) or split across one fast link's nodes (each its share); the answer recommends one",
      "enum": [
        "copies",
        "split"
      ],
      "type": "string"
    },
    "preset": {
      "description": "Serving preset (overhead, model id)",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `delete_model`

```text
Delete a downloaded model and, by default, its kagent ModelConfig.

lmstudio answers unsupported (LM Studio removes models only with `lms rm` on the host): check list_backends' delete capability, and use unwire_model there to drop only the ModelConfig.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend holding the model; without it the model is resolved across backends, as list_backends names it; optional with one backend",
      "type": "string"
    },
    "dryRun": {
      "description": "Write nothing; return the plan: whether the model is loaded and the ModelConfig the delete would unwire",
      "type": "boolean"
    },
    "mode": {
      "description": "apply only: mode commit answers unsupported, naming what to do instead",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "model": {
      "description": "Model reference",
      "type": "string"
    },
    "unwire": {
      "description": "Also remove the ModelConfig (default true)",
      "type": "boolean"
    }
  },
  "required": [
    "model"
  ],
  "type": "object"
}
```

## `get_backend`

```text
Report one serving backend (default: the first configured): health, version, load semantics and capability flags.

Clients must honor the capability flags.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to describe, as list_backends names it; optional with one backend",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `get_info`

```text
Report this server's build, its tool names, the configured backends and, with agent wiring on, where ModelConfigs go.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `get_job`

```text
Get one job: backend, phase, progress, error, and the ModelConfig created on success.
```

```json
{
  "properties": {
    "id": {
      "description": "Job id from pull_model",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `get_model`

```text
Get one downloaded model with its loaded state and kagent ModelConfig.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend holding the model; else resolved across backends (conflict when several hold it), as list_backends names it; optional with one backend",
      "type": "string"
    },
    "model": {
      "description": "Model reference, e.g. smollm2:135m or hf.co/org/repo:Q4_K_M",
      "type": "string"
    }
  },
  "required": [
    "model"
  ],
  "type": "object"
}
```

## `list_backends`

```text
List the serving backends this model-manager runs, in order (the first is the default), with health and capabilities.

Call it first: most tools take an optional backend. An empty list comes with backendsReason: no backend is registered and how to register one. A ModelConfig labelled with a backend not listed is left over from one that went.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `list_jobs`

```text
List pull and load jobs, newest first, with backend, phase and progress.

On kserve a pull job carries the node whose cache receives the download and the preset it is for.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to list jobs of, as list_backends names it; optional with one backend",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `list_loaded_models`

```text
List the models loaded or serving now, with memory use and expiry; on kserve phase, steps, API interfaces and endpoint.

Not read-only on kserve: a served model model-manager manages that has no ModelConfig is wired by this read, as you (wiring.reason "wired on read"). A Ready kserve model carries runtime and interfaces, the APIs its server registered ([] with interfacesReason when unreadable); on the platform's LLM endpoint publicName is the model name clients send there. gitops names the Flux object applying a serving object from git: change it there.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to list, as list_backends names it; optional with one backend",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `list_models`

```text
List downloaded models with backend, size, family and parameters, whether each is loaded, and its kagent ModelConfig.

Without backend every backend is listed; one that fails to answer is reported under errors while the others' models are returned.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to list, as list_backends names it; optional with one backend",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `list_nodes`

```text
List the nodes a backend serves on, with memory budget, what loaded models reserve and the download cache each holds.

kserve: accelerator nodes only; eligible and eligibilityReason say whether a model can be served there, and load_model, pull_model and check_fit refuse an ineligible node with that reason. modelImageEligible says the same for a preset served from an OCI model image. ollama and lemonade: the proxied host, always eligible. lmstudio answers unsupported: LM Studio exposes no host hardware.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to list nodes of, as list_backends names it; optional with one backend",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `list_presets`

```text
List the kserve serving presets, the only way to serve a model there: model, runtime, GPUs and memory requirements.

A preset states no API interfaces; list_loaded_models reads them from the running server.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to list presets of, as list_backends names it; optional with one backend",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `load_model`

```text
Load a model into memory, or on kserve serve it from a preset after a fit check; auto-wire writes its ModelConfig.

kserve: refused with unavailable, nothing created, without the llm-d control plane, and refused before anything is created where no node (or GPU pool size) can host the preset. With auto-wire on kserve the ModelConfig is written before the model is ready and a load job follows it to readiness. A preset served already answers alreadyServing and servingNodes; placement copies on more nodes adds copies there, another node is conflict. mode commit is kserve only.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend holding the model; without it the model is resolved across backends, as list_backends names it; optional with one backend",
      "type": "string"
    },
    "branch": {
      "description": "mode commit: the base branch of repository",
      "type": "string"
    },
    "dryRun": {
      "description": "Write nothing; return the plan: the fit verdict, the serving object it would create or where it serves already, and the ModelConfig it would wire",
      "type": "boolean"
    },
    "keepAlive": {
      "description": "Keep the model loaded this long after the last request: ollama a duration (10m) or -1 forever; lemonade only -1 (pins it); lmstudio ignores it",
      "type": "string"
    },
    "mode": {
      "description": "apply (default): write live as you (gitops_owned where Flux applies it from git). commit: open a pull request as you in its GitOps repository",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "model": {
      "description": "Model reference (required unless preset is given)",
      "type": "string"
    },
    "node": {
      "description": "kserve: pin the workload to this node",
      "type": "string"
    },
    "nodes": {
      "description": "kserve: split, the nodes in rank order, the first serving the API (default: the first fast link hosting the model); copies, one copy on each",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "path": {
      "description": "mode commit: the directory in repository a Flux Kustomization builds from (default: the root); model-manager's files go under <path>/model-manager/",
      "type": "string"
    },
    "placement": {
      "description": "kserve: copies (default: one per node in nodes, else on node or the fit check's pick) or split: one model across one fast link's nodes",
      "enum": [
        "copies",
        "split"
      ],
      "type": "string"
    },
    "preset": {
      "description": "kserve: serving preset to compose the LLMInferenceService from; default: the single preset serving the model",
      "type": "string"
    },
    "repository": {
      "description": "mode commit: the repository (owner/name) if no Flux provenance names one; branch and path with it override that (remote kserve: serving object only)",
      "type": "string"
    },
    "tracing": {
      "description": "kserve: export the served model's traces under the caller's trace (default false: costs throughput); switching on a served preset answers conflict",
      "type": "boolean"
    }
  },
  "required": [],
  "type": "object"
}
```

## `pull_model`

```text
Download a model onto a backend: start a pull job and return at once; poll get_job for progress.

The reference is an Ollama tag or hf.co/… GGUF (ollama), a Lemonade catalog name such as Qwen3-0.6B-GGUF (lemonade), an LM Studio hub reference such as ibm/granite-4-micro (lmstudio), or a Hugging Face repository owner/name (kserve: a download Job into a node's cache after a fit check). On ollama, lemonade and lmstudio the model is wired into kagent on success unless wire=false; on kserve it is wired when served.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend to pull on; default: the first configured, as list_backends names it; optional with one backend",
      "type": "string"
    },
    "dryRun": {
      "description": "Write nothing; return the plan: whether the model is there, on kserve the fit verdict and target node, and the ModelConfig it would wire",
      "type": "boolean"
    },
    "mode": {
      "description": "apply only: mode commit answers unsupported, naming what to do instead",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "model": {
      "description": "Model reference to pull",
      "type": "string"
    },
    "node": {
      "description": "kserve: node whose cache receives the download; default: the cache node or the node with the largest budget",
      "type": "string"
    },
    "preset": {
      "description": "kserve: the serving preset the download is for, whose serving object mounts the cache; default: the one preset serving the model",
      "type": "string"
    },
    "wire": {
      "description": "Create a kagent ModelConfig when the pull completes (ollama, lemonade, lmstudio; default: the server's autoWire setting)",
      "type": "boolean"
    }
  },
  "required": [
    "model"
  ],
  "type": "object"
}
```

## `remove_backend`

```text
Remove a backend registered at runtime: unwire model-manager's ModelConfigs for it, then delete its backend document.

Refused with conflict, nothing removed, while a ModelConfig of the backend no model-manager deletes (no instance label, or applied by Flux from git) would be left pointing at nothing; the refusal names each. Its served models keep running: unload them first. A backend the chart configures cannot be removed here. dryRun reports what would go and blockedBy.
```

```json
{
  "properties": {
    "dryRun": {
      "description": "Report the ConfigMap and ModelConfigs that would be removed (default false)",
      "type": "boolean"
    },
    "kind": {
      "description": "Backend kind, the name of a backend registered under its kind (ollama|lmstudio|lemonade|kserve)",
      "type": "string"
    },
    "mode": {
      "description": "apply (default): delete as you. commit is not available yet",
      "type": "string"
    },
    "name": {
      "description": "Backend name as list_backends shows it (kserve, kserve-<cluster>, ollama, ...); required unless kind is given",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `search_models`

```text
Search the model hub (kserve: Hugging Face Hub) by free text; each hit with gated and private flags and its presets.

Run check_fit before pulling.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend whose hub to search, as list_backends names it; optional with one backend",
      "type": "string"
    },
    "limit": {
      "description": "Maximum results (default 20, max 50)",
      "type": "number"
    },
    "query": {
      "description": "Search text",
      "type": "string"
    }
  },
  "required": [
    "query"
  ],
  "type": "object"
}
```

## `unload_model`

```text
Unload a model from memory, or on kserve stop serving it and unwire its ModelConfig; the download stays.

On kserve only a ModelConfig this model-manager created is unwired; any other is named in modelConfigLeft. next says what list_loaded_models shows meanwhile. mode commit is kserve only.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend holding the model; without it the model is resolved across backends, as list_backends names it; optional with one backend",
      "type": "string"
    },
    "branch": {
      "description": "mode commit: the base branch of repository",
      "type": "string"
    },
    "dryRun": {
      "description": "Write nothing; return the plan: on kserve the serving objects it would delete and the ModelConfig to unwire, elsewhere whether it is loaded",
      "type": "boolean"
    },
    "mode": {
      "description": "apply (default): write live as you (gitops_owned where Flux applies it from git). commit: open a pull request as you in its GitOps repository",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "model": {
      "description": "Model reference",
      "type": "string"
    },
    "path": {
      "description": "mode commit: the directory in repository a Flux Kustomization builds from (default: the root); model-manager's files go under <path>/model-manager/",
      "type": "string"
    },
    "repository": {
      "description": "mode commit: the repository (owner/name) if no Flux provenance names one; branch and path with it override that (remote kserve: serving object only)",
      "type": "string"
    }
  },
  "required": [
    "model"
  ],
  "type": "object"
}
```

## `unwire_model`

```text
Delete the kagent ModelConfig model-manager created for a model, and its placeholder Secret; the model stays.

Also removes a leftover without an instance label (written before model-manager recorded its creator) once no Agent, AgentTemplate or Harness references it; referenced, it answers conflict naming them. A ModelConfig Flux applies from git is never deleted live: gitops_owned names the Flux object.
```

```json
{
  "properties": {
    "backend": {
      "description": "Backend the ModelConfig belongs to; else found among the wired ones (conflict when ambiguous), as list_backends names it; optional with one backend",
      "type": "string"
    },
    "branch": {
      "description": "mode commit: the base branch of repository",
      "type": "string"
    },
    "dryRun": {
      "description": "Write nothing; return the plan: the ModelConfig and Secret it would delete and the Flux object applying the ModelConfig from git",
      "type": "boolean"
    },
    "mode": {
      "description": "apply (default): write live as you (gitops_owned where Flux applies it from git). commit: open a pull request as you in its GitOps repository",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "model": {
      "description": "Model reference",
      "type": "string"
    },
    "path": {
      "description": "mode commit: the directory in repository a Flux Kustomization builds from (default: the root); model-manager's files go under <path>/model-manager/",
      "type": "string"
    },
    "repository": {
      "description": "mode commit: the repository (owner/name) if no Flux provenance names one; branch and path with it override that (remote kserve: serving object only)",
      "type": "string"
    }
  },
  "required": [
    "model"
  ],
  "type": "object"
}
```

## `wire_model`

```text
Create or refresh the kagent ModelConfig for a downloaded model so agents can use it.

The backend decides how the ModelConfig authenticates (a kserve model on the models Gateway forwards the caller's token, a keyless in-cluster endpoint gets kagent's placeholder key) unless apiKeyPassthrough or apiKeySecret says otherwise; the two are mutually exclusive. A ModelConfig Flux applies from git is never written live: gitops_owned names the Flux object.
```

```json
{
  "properties": {
    "apiKeyPassthrough": {
      "description": "Forward the Bearer token of the agent's request as the API key, for an endpoint that admits the person's token (the models Gateway); no Secret",
      "type": "boolean"
    },
    "apiKeySecret": {
      "description": "Name of an existing Secret in the kagent namespace holding a static API key the endpoint checks; model-manager never creates or deletes it",
      "type": "string"
    },
    "apiKeySecretKey": {
      "description": "Key within apiKeySecret holding the API key (default OPENAI_API_KEY)",
      "type": "string"
    },
    "backend": {
      "description": "Backend holding the model; without it the model is resolved across backends, as list_backends names it; optional with one backend",
      "type": "string"
    },
    "branch": {
      "description": "mode commit: the base branch of repository",
      "type": "string"
    },
    "dryRun": {
      "description": "Write nothing; return the plan: the manifests, the Flux object applying the ModelConfig from git, another owner's ModelConfig wiring it",
      "type": "boolean"
    },
    "mode": {
      "description": "apply (default): write live as you (gitops_owned where Flux applies it from git). commit: open a pull request as you in its GitOps repository",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "model": {
      "description": "Model reference",
      "type": "string"
    },
    "path": {
      "description": "mode commit: the directory in repository a Flux Kustomization builds from (default: the root); model-manager's files go under <path>/model-manager/",
      "type": "string"
    },
    "repository": {
      "description": "mode commit: the repository (owner/name) if no Flux provenance names one; branch and path with it override that (remote kserve: serving object only)",
      "type": "string"
    }
  },
  "required": [
    "model"
  ],
  "type": "object"
}
```
