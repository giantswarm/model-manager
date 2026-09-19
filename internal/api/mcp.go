package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/registry"
	"github.com/giantswarm/model-manager/internal/service"
)

// MCP tool names. Through muster they appear as x_<server>_<tool>, e.g.
// x_model-manager_list_models.
const (
	ToolGetInfo          = "get_info"
	ToolGetBackend       = "get_backend"
	ToolListBackends     = "list_backends"
	ToolListModels       = "list_models"
	ToolGetModel         = "get_model"
	ToolListLoadedModels = "list_loaded_models"
	ToolPullModel        = "pull_model"
	ToolLoadModel        = "load_model"
	ToolUnloadModel      = "unload_model"
	ToolDeleteModel      = "delete_model"
	ToolWireModel        = "wire_model"
	ToolUnwireModel      = "unwire_model"
	ToolListJobs         = "list_jobs"
	ToolGetJob           = "get_job"
	ToolCancelJob        = "cancel_job"
	// kserve capabilities; unsupported on other backends.
	ToolListPresets  = "list_presets"
	ToolSearchModels = "search_models"
	ToolCheckFit     = "check_fit"
	ToolListNodes    = "list_nodes"
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{
		ToolGetInfo, ToolGetBackend, ToolListBackends, ToolListModels, ToolGetModel, ToolListLoadedModels,
		ToolPullModel, ToolLoadModel, ToolUnloadModel, ToolDeleteModel,
		ToolWireModel, ToolUnwireModel, ToolListJobs, ToolGetJob, ToolCancelJob,
		ToolListPresets, ToolSearchModels, ToolCheckFit, ToolListNodes,
		ToolAddBackend, ToolRemoveBackend,
	}
}

const (
	argBackend   = "backend"
	argModel     = "model"
	argWire      = "wire"
	argKeepAlive = "keepAlive"
	argUnwire    = "unwire"
	// The API-key shape of a wired ModelConfig (wire_model).
	argAPIKeyPassthrough = "apiKeyPassthrough" // #nosec G101 -- argument name, not a credential
	argAPIKeySecret      = "apiKeySecret"      // #nosec G101 -- argument name, not a credential
	argAPIKeySecretKey   = "apiKeySecretKey"   // #nosec G101 -- argument name, not a credential
	argJobID             = "id"
	argPreset            = "preset"
	argNode              = "node"
	argQuery             = "query"
	argLimit             = "limit"
)

// backendArg is the optional backend argument every tool takes.
func backendArg(what string) mcp.ToolOption {
	return mcp.WithString(argBackend, mcp.Description("Backend (ollama|kserve|lemonade|lmstudio) "+what+"; one model-manager may run several — list_backends names them. Optional when one backend is configured."))
}

// Info is get_info's answer: the build this server runs, the tools it
// registers, the backends it holds and where it wires models into kagent.
type Info struct {
	// Version is the release (the image tag), dev for an untagged local
	// build; Commit and Built identify the source and the build time.
	Version string   `json:"version"`
	Commit  string   `json:"commit"`
	Built   string   `json:"built"`
	Tools   []string `json:"tools"`
	// Backends names every configured backend in order, the first being the
	// default; empty on an installation that has registered none yet.
	Backends []backend.Name `json:"backends"`
	// Wiring describes where ModelConfigs are created; absent when agent
	// wiring is disabled.
	Wiring *service.WiringInfo `json:"wiring,omitempty"`
}

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
// build is what get_info and the MCP server identity report as the version.
func NewMCPServer(svc *service.Service, build buildinfo.Info, opts ...Option) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("model-manager", build.Version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the models one or several serving backends (ollama, kserve, lemonade, lmstudio) hold: list downloaded and loaded models, pull with progress, load/unload, delete, and wire models into kagent ModelConfigs so agents can use them. Backends are registered at runtime with add_backend (remove_backend drops one); an installation may run none yet, and list_backends is then empty. Call list_backends first to learn which backends this installation runs and which capabilities each supports; every model carries its backend, and every tool takes an optional backend argument — required when the same model reference exists on several backends (the tool then answers conflict). On kserve also use list_presets, search_models, check_fit and list_nodes before pulling or loading."),
	)
	t := &tools{svc: svc, build: build}
	for _, o := range opts {
		o(t)
	}
	t.registerBackendTools(s)

	s.AddTool(mcp.NewTool(ToolGetInfo,
		mcp.WithDescription("Report this server's build (version — the release, or dev for a local build —, commit and build time), the names of its tools, the configured backends in order (the first is the default) and, when agent wiring is enabled, the namespace and kagent API version ModelConfigs are written to."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolGetBackend,
		mcp.WithDescription("Report one serving backend (ollama|kserve|lemonade|lmstudio) — the named one, else the default (first configured) — with its health/version, its load semantics, the capability flags clients must honor, and the names of every configured backend."),
		backendArg("to describe"),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getBackend)

	s.AddTool(mcp.NewTool(ToolListBackends,
		mcp.WithDescription("List every serving backend this model-manager runs, in configured order (the first is the default backend an unqualified pull goes to), each with its health, endpoints, load semantics and capability flags."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listBackends)

	s.AddTool(mcp.NewTool(ToolListModels,
		mcp.WithDescription("List downloaded models with their backend, size, family/parameters, whether each is loaded, and its kagent ModelConfig if wired. Without a backend every backend is listed; a backend that fails to answer is reported under errors while the others' models are returned."),
		backendArg("to list"),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listModels)

	s.AddTool(mcp.NewTool(ToolGetModel,
		mcp.WithDescription("Get one downloaded model with loaded state and ModelConfig reference."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference, e.g. smollm2:135m or hf.co/org/repo:Q4_K_M")),
		backendArg("holding the model; without it the model is resolved across backends (conflict when several hold it)"),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getModel)

	s.AddTool(mcp.NewTool(ToolListLoadedModels,
		mcp.WithDescription("List models currently loaded in memory / serving, with their backend, memory use and expiry. On kserve every entry carries its phase and steps[] and, when one exists, its kagent ModelConfig (modelConfig); a served model model-manager manages (managedBy model-manager) that has no ModelConfig is wired by this read as the caller, and the entry says so (wiring: {wired: true, reason: \"wired on read\", modelConfig}) — the mend for a model-manager restart during a cold start, since no reconciler runs without a caller."),
		backendArg("to list"),
		mcp.WithIdempotentHintAnnotation(true),
	), t.listLoaded)

	s.AddTool(mcp.NewTool(ToolPullModel,
		mcp.WithDescription("Start importing a model: an Ollama registry tag or hf.co/... GGUF reference (ollama), a Lemonade catalog model name such as Qwen3-0.6B-GGUF (lemonade: `lemonade list`, GET /api/v1/models?show_all=true on the server), an LM Studio hub reference such as ibm/granite-4-micro (lmstudio: `lms ls`), or a Hugging Face repository owner/name (kserve: a pre-warm download Job into the node cache after a fit check). Returns a job immediately; poll get_job for progress. On ollama, lemonade and lmstudio the model is wired into kagent on success unless wire=false; on kserve models are wired when served."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference to pull")),
		backendArg("to pull on; default: the default (first configured) backend"),
		mcp.WithBoolean(argWire, mcp.Description("Create a kagent ModelConfig when the pull completes (ollama, lemonade, lmstudio; default: the server's autoWire setting)")),
		mcp.WithString(argPreset, mcp.Description("kserve: serving preset the download is for (its LLMInferenceService mounts the resulting cache directory); default: the single preset serving the model")),
		mcp.WithString(argNode, mcp.Description("kserve: node whose cache receives the download; default: the cache node or the node with the largest budget")),
	), t.pull)

	s.AddTool(mcp.NewTool(ToolLoadModel,
		mcp.WithDescription("Load a downloaded model into memory (ollama, lemonade, lmstudio) / start serving it as an LLMInferenceService composed from a serving preset after a fit check (kserve) — refused with `unavailable` and nothing created where the llm-d control plane is not installed; a preset no node — or, on a GPU pool with no node yet whose instance shapes are known, no size of the pool — can host is refused before any object is created. On kserve the answer carries fit (the verdict), running (the serving object, its phase and steps[]) and wiring: the kagent ModelConfig created in the same call, before the model is ready, at the address the model will answer on — apiKeyPassthrough for a model routed on the models Gateway — so an agent created on it answers as soon as the model serves; a `load` job follows the model to readiness and refreshes that ModelConfig from the published address. On lemonade keepAlive -1 pins the model against slot eviction; Lemonade has no idle timer, so other keep-alives are ignored. On lmstudio a load persists until unloaded — no TTL, no keep-alive; a model an agent JIT-loaded instead gets LM Studio's idle TTL."),
		mcp.WithString(argModel, mcp.Description("Model reference (required unless preset is given)")),
		backendArg("holding the model; without it the model is resolved across backends"),
		mcp.WithString(argKeepAlive, mcp.Description("How long to keep the model loaded after the last request (ollama duration such as 10m, or -1 for forever; lemonade: only -1 means something — it pins the model; lmstudio has neither timer nor pinning, so keep-alives are ignored)")),
		mcp.WithString(argPreset, mcp.Description("kserve: serving preset to compose the LLMInferenceService from; default: the single preset serving the model")),
		mcp.WithString(argNode, mcp.Description("kserve: pin the workload to this node")),
		mcp.WithIdempotentHintAnnotation(true),
	), t.load)

	s.AddTool(mcp.NewTool(ToolListPresets,
		mcp.WithDescription("List the curated serving presets (kserve): model, runtime, GPUs, weights and overhead requirements, arguments. Presets are the only way to serve a model on kserve."),
		backendArg("to list presets of"),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listPresets)

	s.AddTool(mcp.NewTool(ToolSearchModels,
		mcp.WithDescription("Search the model hub (kserve: Hugging Face Hub) by free text. Results carry gated/private flags and the presets that serve each hit; run check_fit before pulling."),
		mcp.WithString(argQuery, mcp.Required(), mcp.Description("Search text")),
		backendArg("whose hub to search"),
		mcp.WithNumber(argLimit, mcp.Description("Maximum results (default 20, max 50)")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.search)

	s.AddTool(mcp.NewTool(ToolCheckFit,
		mcp.WithDescription("Check whether a model fits a node (kserve): resolves the weight size from the hub (the safetensors index's total_size while the shards its weight_map names agree with it within one percent, else those shards; else the file tree; else the preset), adds the serving overhead and compares with the node's memory budget. Says which node, whether the model is cached there and whether a hub token is needed. When the hub does not answer within its lookup timeout, the preset's requirements size the model (weightsSource preset) and reason says so. A GPU pool with no node yet (budgetSource pool-scale-from-zero) is judged on its instance shapes when the backend document lists them: the preset's CPU/memory requests, GPUs and GPU memory — budgeted from the GPUs the preset requests on a size, not the node's — against the pool's sizes, instanceType naming the size the node will come as, or fits false naming what no size of the pool leaves; declaredWeightsBytes carries the preset's declared weights beside the hub-sized weightsBytes, and reason names the discrepancy on both verdicts when the hub holds more; without shapes the answer is yes and unverified."),
		mcp.WithString(argModel, mcp.Description("Hugging Face repository owner/name (required unless preset is given)")),
		backendArg("to check on (required when several backends offer fit checks)"),
		mcp.WithString(argPreset, mcp.Description("Serving preset (overhead, model id)")),
		mcp.WithString(argNode, mcp.Description("Node to check against; default: the best eligible node")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.checkFit)

	s.AddTool(mcp.NewTool(ToolListNodes,
		mcp.WithDescription("List nodes with their backend, serving memory budget, what loaded models reserve and the download cache each node holds. kserve: the accelerator nodes only (GPU resource or gpu-feature-discovery labels), budget from GPU labels or allocatable memory, and eligible / eligibilityReason saying whether a model can be served there (ready, inside the serving node selector, able to mount the cache claim) — load_model, pull_model and check_fit refuse a node with eligible=false and echo the reason. ollama: the proxied host (always eligible), budget from MemTotal of /proc/meminfo as the pod sees it or the operator's ollama.memoryBudgetGiB override (budgetSource says which), reservations from /api/ps, accelerated when a loaded model sits on the GPU. lemonade: the proxied host as Lemonade's system-info reports it (always eligible) — budget from the host memory (budgetSource system-info), gpuCount/gpuProduct from the accelerators Lemonade enumerates (the NPU, the GPUs), reservations = the catalog sizes of the loaded models, and the model store as the cache. lmstudio reports no nodes at all — LM Studio exposes no host hardware, so the call answers 501 unsupported."),
		backendArg("to list nodes of"),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listNodes)

	s.AddTool(mcp.NewTool(ToolUnloadModel,
		mcp.WithDescription("Unload a model from memory / stop serving it. The download stays. On kserve the serving object is deleted and its ModelConfig unwired within the call, whatever the cache scan would take; the answer's inventory says whether the cache is rescanned in the background or why it cannot be, and next says what list_loaded_models shows meanwhile."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("holding the model; without it the model is resolved across backends"),
		mcp.WithIdempotentHintAnnotation(true),
	), t.unload)

	s.AddTool(mcp.NewTool(ToolDeleteModel,
		mcp.WithDescription("Delete a downloaded model and, by default, its kagent ModelConfig. Not every backend can: lmstudio answers 501 unsupported (LM Studio removes models only through its `lms rm` CLI on the host) — check the delete capability from list_backends, and use unwire_model there to drop just the ModelConfig."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("holding the model; without it the model is resolved across backends"),
		mcp.WithBoolean(argUnwire, mcp.Description("Also remove the ModelConfig (default true)")),
		mcp.WithDestructiveHintAnnotation(true),
	), t.deleteModel)

	s.AddTool(mcp.NewTool(ToolWireModel,
		mcp.WithDescription("Create (or refresh) the kagent ModelConfig for a downloaded model so agents can use it. The backend decides how the ModelConfig authenticates against the endpoint — a kserve model routed on the models Gateway forwards the caller's own token (apiKeyPassthrough), a keyless in-cluster endpoint gets kagent's placeholder key — unless apiKeyPassthrough or apiKeySecret says otherwise; the two are mutually exclusive."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("holding the model; without it the model is resolved across backends"),
		mcp.WithBoolean(argAPIKeyPassthrough, mcp.Description("Forward the Bearer token of the agent's incoming request to the endpoint as the API key (for an endpoint that admits the person's token, such as the models Gateway); no Secret is referenced or created")),
		mcp.WithString(argAPIKeySecret, mcp.Description("Name of an existing Secret in the kagent namespace holding a static API key the endpoint checks; model-manager never creates or deletes it")),
		mcp.WithString(argAPIKeySecretKey, mcp.Description("Key within apiKeySecret holding the API key (default OPENAI_API_KEY)")),
		mcp.WithIdempotentHintAnnotation(true),
	), t.wire)

	s.AddTool(mcp.NewTool(ToolUnwireModel,
		mcp.WithDescription("Delete the kagent ModelConfig model-manager created for a model. The model itself stays."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("the ModelConfig belongs to; without it the wired ModelConfigs are consulted (conflict when several backends wire the reference)"),
		mcp.WithIdempotentHintAnnotation(true),
	), t.unwire)

	s.AddTool(mcp.NewTool(ToolListJobs,
		mcp.WithDescription("List jobs (newest first) with their backend, phase and progress; on kserve a pull job carries the node whose cache receives the download and the serving preset it is for (ollama, lemonade and lmstudio jobs carry neither)."),
		backendArg("to list jobs of"),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listJobs)

	s.AddTool(mcp.NewTool(ToolGetJob,
		mcp.WithDescription("Get one job: backend, phase (pending|running|succeeded|failed|cancelled), bytes done/total, percent, error, and the ModelConfig created on success."),
		mcp.WithString(argJobID, mcp.Required(), mcp.Description("Job id from pull_model")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getJob)

	s.AddTool(mcp.NewTool(ToolCancelJob,
		mcp.WithDescription("Cancel a running job."),
		mcp.WithString(argJobID, mcp.Required(), mcp.Description("Job id")),
	), t.cancelJob)

	return s
}

type tools struct {
	svc   *service.Service
	build buildinfo.Info
	store *registry.Store // nil: no Kubernetes access, registration tools refuse
}

func (t *tools) getInfo(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	names := t.svc.Names()
	if names == nil {
		names = []backend.Name{}
	}
	return jsonResult(Info{Version: t.build.Version, Commit: t.build.Commit, Built: t.build.Date, Tools: ToolNames(), Backends: names, Wiring: t.svc.Wiring()})
}

// withErrorsResult adds the per-backend failures of an aggregate read.
func withErrorsResult(body map[string]any, errs service.Errors) (*mcp.CallToolResult, error) {
	return jsonResult(withErrors(body, errs))
}

func (t *tools) getBackend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := t.svc.Backend(ctx, req.GetString(argBackend, ""))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(resp)
}

func (t *tools) listBackends(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	out := map[string]any{"backends": t.svc.Backends(ctx)}
	if invalid := t.svc.InvalidDocuments(); len(invalid) > 0 {
		out["invalid"] = invalid
	}
	return jsonResult(out)
}

func (t *tools) listModels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	models, errs, err := t.svc.ListModels(ctx, req.GetString(argBackend, ""))
	if err != nil {
		return errResult(err), nil
	}
	return withErrorsResult(map[string]any{"models": models}, errs)
}

func (t *tools) getModel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	m, err := t.svc.GetModel(ctx, req.GetString(argBackend, ""), name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(m)
}

func (t *tools) listLoaded(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	loaded, errs, err := t.svc.ListLoaded(ctx, req.GetString(argBackend, ""))
	if err != nil {
		return errResult(err), nil
	}
	return withErrorsResult(map[string]any{"loaded": loaded}, errs)
}

func (t *tools) pull(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	var wire *bool
	if raw, ok := req.GetArguments()[argWire]; ok {
		if b, ok := raw.(bool); ok {
			wire = &b
		}
	}
	job, created, err := t.svc.Pull(ctx, service.PullOptions{Backend: req.GetString(argBackend, ""), Model: name, Wire: wire, Preset: req.GetString(argPreset, ""), Node: req.GetString(argNode, "")})
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"job": job, "created": created})
}

func (t *tools) load(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString(argModel, "")
	preset := req.GetString(argPreset, "")
	if name == "" && preset == "" {
		return errResult(fmt.Errorf("%w: model or preset is required", backend.ErrInvalid)), nil
	}
	m, err := t.svc.Load(ctx, service.LoadOptions{Backend: req.GetString(argBackend, ""), Model: name, KeepAlive: req.GetString(argKeepAlive, ""), Preset: preset, Node: req.GetString(argNode, "")})
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(m)
}

func (t *tools) listPresets(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	presets, errs, err := t.svc.Presets(ctx, req.GetString(argBackend, ""))
	if err != nil {
		return errResult(err), nil
	}
	return withErrorsResult(map[string]any{"presets": presets}, errs)
}

func (t *tools) search(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString(argQuery)
	if err != nil {
		return errResult(err), nil
	}
	hits, errs, err := t.svc.Search(ctx, req.GetString(argBackend, ""), query, req.GetInt(argLimit, 0))
	if err != nil {
		return errResult(err), nil
	}
	return withErrorsResult(map[string]any{"query": query, "results": hits}, errs)
}

func (t *tools) checkFit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := t.svc.FitCheck(ctx, req.GetString(argBackend, ""), backend.FitRequest{Model: req.GetString(argModel, ""), Preset: req.GetString(argPreset, ""), Node: req.GetString(argNode, "")})
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) listNodes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	nodes, errs, err := t.svc.Nodes(ctx, req.GetString(argBackend, ""))
	if err != nil {
		return errResult(err), nil
	}
	return withErrorsResult(map[string]any{"nodes": nodes}, errs)
}

func (t *tools) unload(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	view, err := t.svc.Unload(ctx, req.GetString(argBackend, ""), name)
	if err != nil {
		return errResult(err), nil
	}
	out := map[string]any{argBackend: view.Backend, argModel: name, "loaded": false}
	if view.Backend == backend.NameKServe {
		// The serving object is deleted, not gone: the list shows it as
		// Terminating until Kubernetes has removed it and its pod.
		out["status"] = "Terminating"
		next := "the serving object is being deleted; list_loaded_models shows it with status Terminating (phase terminating) until it is gone, then no longer"
		if inv := view.Inventory; inv != nil {
			out["inventory"] = inv
			if inv.Refreshing {
				next += "; the cache inventory is rescanned in the background — list_models answers from the new scan once it is done"
			} else {
				next += "; the cache inventory is not rescanned (" + inv.Reason + ") — list_models answers from the last scan"
			}
		}
		out["next"] = next
	}
	return jsonResult(out)
}

func (t *tools) deleteModel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	unwire := req.GetBool(argUnwire, true)
	b, err := t.svc.Delete(ctx, req.GetString(argBackend, ""), name, unwire)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{argBackend: b, argModel: name, "deleted": true, "unwired": unwire})
}

func (t *tools) wire(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	opts := backend.WireOptions{
		APIKeyPassthrough: req.GetBool(argAPIKeyPassthrough, false),
		APIKeySecret:      req.GetString(argAPIKeySecret, ""),
		APIKeySecretKey:   req.GetString(argAPIKeySecretKey, ""),
	}
	ref, err := t.svc.Wire(ctx, req.GetString(argBackend, ""), name, opts)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{argBackend: ref.Backend, argModel: name, "modelConfig": ref})
}

func (t *tools) unwire(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	b, err := t.svc.Unwire(ctx, req.GetString(argBackend, ""), name)
	if err != nil {
		return errResult(err), nil
	}
	body := map[string]any{argModel: name, "modelConfig": nil}
	if b != "" {
		body[argBackend] = b
	}
	return jsonResult(body)
}

func (t *tools) listJobs(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	list, err := t.svc.Jobs(req.GetString(argBackend, ""))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"jobs": list})
}

func (t *tools) getJob(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argJobID)
	if err != nil {
		return errResult(err), nil
	}
	job, err := t.svc.Job(id)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(job)
}

func (t *tools) cancelJob(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argJobID)
	if err != nil {
		return errResult(err), nil
	}
	job, err := t.svc.CancelJob(id)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(job)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func errResult(err error) *mcp.CallToolResult {
	_, code := statusFor(err)
	return mcp.NewToolResultError(fmt.Sprintf("%s: %s", code, err.Error()))
}
