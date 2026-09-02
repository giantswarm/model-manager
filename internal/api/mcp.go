package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/service"
)

// MCP tool names. Through muster they appear as x_<server>_<tool>, e.g.
// x_model-manager_list_models.
const (
	ToolGetBackend       = "get_backend"
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
		ToolGetBackend, ToolListModels, ToolGetModel, ToolListLoadedModels,
		ToolPullModel, ToolLoadModel, ToolUnloadModel, ToolDeleteModel,
		ToolWireModel, ToolUnwireModel, ToolListJobs, ToolGetJob, ToolCancelJob,
		ToolListPresets, ToolSearchModels, ToolCheckFit, ToolListNodes,
	}
}

const (
	argModel     = "model"
	argWire      = "wire"
	argKeepAlive = "keepAlive"
	argUnwire    = "unwire"
	argJobID     = "id"
	argPreset    = "preset"
	argNode      = "node"
	argQuery     = "query"
	argLimit     = "limit"
)

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
func NewMCPServer(svc *service.Service, version string) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("model-manager", version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the models a serving backend (ollama or kserve) holds: list downloaded and loaded models, pull with progress, load/unload, delete, and wire models into kagent ModelConfigs so agents can use them. Call get_backend first to learn which capabilities this installation supports; on kserve also use list_presets, search_models, check_fit and list_nodes before pulling or loading."),
	)
	t := &tools{svc: svc}

	s.AddTool(mcp.NewTool(ToolGetBackend,
		mcp.WithDescription("Report the serving backend (ollama|kserve), its health/version and the capability flags clients must honor."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getBackend)

	s.AddTool(mcp.NewTool(ToolListModels,
		mcp.WithDescription("List downloaded models with size, family/parameters, whether each is loaded, and its kagent ModelConfig if wired."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listModels)

	s.AddTool(mcp.NewTool(ToolGetModel,
		mcp.WithDescription("Get one downloaded model with loaded state and ModelConfig reference."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference, e.g. smollm2:135m or hf.co/org/repo:Q4_K_M")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.getModel)

	s.AddTool(mcp.NewTool(ToolListLoadedModels,
		mcp.WithDescription("List models currently loaded in memory / serving, with memory use and expiry."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listLoaded)

	s.AddTool(mcp.NewTool(ToolPullModel,
		mcp.WithDescription("Start importing a model: an Ollama registry tag or hf.co/... GGUF reference (ollama), or a Hugging Face repository owner/name (kserve: a pre-warm download Job into the node cache after a fit check). Returns a job immediately; poll get_job for progress. On ollama the model is wired into kagent on success unless wire=false; on kserve models are wired when served."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference to pull")),
		mcp.WithBoolean(argWire, mcp.Description("Create a kagent ModelConfig when the pull completes (ollama; default: the server's autoWire setting)")),
		mcp.WithString(argPreset, mcp.Description("kserve: serving preset the download is for (its InferenceService mounts the resulting cache directory); default: the single preset serving the model")),
		mcp.WithString(argNode, mcp.Description("kserve: node whose cache receives the download; default: the cache node or the node with the largest budget")),
	), t.pull)

	s.AddTool(mcp.NewTool(ToolLoadModel,
		mcp.WithDescription("Load a downloaded model into memory (ollama) / start serving it as an InferenceService composed from a serving preset after a fit check (kserve). On kserve a `load` job follows the model to readiness and then wires it into kagent."),
		mcp.WithString(argModel, mcp.Description("Model reference (required unless preset is given)")),
		mcp.WithString(argKeepAlive, mcp.Description("How long to keep the model loaded after the last request (ollama duration such as 10m, or -1 for forever)")),
		mcp.WithString(argPreset, mcp.Description("kserve: serving preset to compose the InferenceService from; default: the single preset serving the model")),
		mcp.WithString(argNode, mcp.Description("kserve: pin the predictor to this node")),
		mcp.WithIdempotentHintAnnotation(true),
	), t.load)

	s.AddTool(mcp.NewTool(ToolListPresets,
		mcp.WithDescription("List the curated serving presets (kserve): model, runtime, GPUs, weights and overhead requirements, arguments. Presets are the only way to serve a model on kserve."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listPresets)

	s.AddTool(mcp.NewTool(ToolSearchModels,
		mcp.WithDescription("Search the model hub (kserve: Hugging Face Hub) by free text. Results carry gated/private flags and the presets that serve each hit; run check_fit before pulling."),
		mcp.WithString(argQuery, mcp.Required(), mcp.Description("Search text")),
		mcp.WithNumber(argLimit, mcp.Description("Maximum results (default 20, max 50)")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.search)

	s.AddTool(mcp.NewTool(ToolCheckFit,
		mcp.WithDescription("Check whether a model fits a node (kserve): resolves the weight size from the hub (safetensors index, else file tree, else the preset), adds the serving overhead and compares with the node's memory budget. Says which node, whether the model is cached there and whether a hub token is needed."),
		mcp.WithString(argModel, mcp.Description("Hugging Face repository owner/name (required unless preset is given)")),
		mcp.WithString(argPreset, mcp.Description("Serving preset (overhead, model id)")),
		mcp.WithString(argNode, mcp.Description("Node to check against; default: the best eligible node")),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.checkFit)

	s.AddTool(mcp.NewTool(ToolListNodes,
		mcp.WithDescription("List nodes with their serving memory budget, what loaded models reserve and the download cache each node holds. kserve: every node, budget from GPU labels or allocatable memory. ollama: the proxied host, budget from MemTotal of /proc/meminfo as the pod sees it, reservations from /api/ps, accelerated when a loaded model sits on the GPU."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listNodes)

	s.AddTool(mcp.NewTool(ToolUnloadModel,
		mcp.WithDescription("Unload a model from memory / stop serving it. The download stays."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		mcp.WithIdempotentHintAnnotation(true),
	), t.unload)

	s.AddTool(mcp.NewTool(ToolDeleteModel,
		mcp.WithDescription("Delete a downloaded model and, by default, its kagent ModelConfig."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		mcp.WithBoolean(argUnwire, mcp.Description("Also remove the ModelConfig (default true)")),
		mcp.WithDestructiveHintAnnotation(true),
	), t.deleteModel)

	s.AddTool(mcp.NewTool(ToolWireModel,
		mcp.WithDescription("Create (or refresh) the kagent ModelConfig for a downloaded model so agents can use it."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		mcp.WithIdempotentHintAnnotation(true),
	), t.wire)

	s.AddTool(mcp.NewTool(ToolUnwireModel,
		mcp.WithDescription("Delete the kagent ModelConfig model-manager created for a model. The model itself stays."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		mcp.WithIdempotentHintAnnotation(true),
	), t.unwire)

	s.AddTool(mcp.NewTool(ToolListJobs,
		mcp.WithDescription("List pull jobs (newest first) with phase and progress."),
		mcp.WithReadOnlyHintAnnotation(true),
	), t.listJobs)

	s.AddTool(mcp.NewTool(ToolGetJob,
		mcp.WithDescription("Get one job: phase (pending|running|succeeded|failed|cancelled), bytes done/total, percent, error, and the ModelConfig created on success."),
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
	svc *service.Service
}

func (t *tools) getBackend(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(t.svc.Backend(ctx))
}

func (t *tools) listModels(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	models, err := t.svc.ListModels(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"models": models})
}

func (t *tools) getModel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	m, err := t.svc.GetModel(ctx, name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(m)
}

func (t *tools) listLoaded(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	loaded, err := t.svc.ListLoaded(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"loaded": loaded})
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
	job, created, err := t.svc.Pull(ctx, service.PullOptions{Model: name, Wire: wire, Preset: req.GetString(argPreset, ""), Node: req.GetString(argNode, "")})
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
	m, err := t.svc.Load(ctx, service.LoadOptions{Model: name, KeepAlive: req.GetString(argKeepAlive, ""), Preset: preset, Node: req.GetString(argNode, "")})
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(m)
}

func (t *tools) listPresets(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	presets, err := t.svc.Presets(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"presets": presets})
}

func (t *tools) search(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString(argQuery)
	if err != nil {
		return errResult(err), nil
	}
	hits, err := t.svc.Search(ctx, query, req.GetInt(argLimit, 0))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"query": query, "results": hits})
}

func (t *tools) checkFit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := t.svc.FitCheck(ctx, backend.FitRequest{Model: req.GetString(argModel, ""), Preset: req.GetString(argPreset, ""), Node: req.GetString(argNode, "")})
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) listNodes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	nodes, err := t.svc.Nodes(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{"nodes": nodes})
}

func (t *tools) unload(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	if err := t.svc.Unload(ctx, name); err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{argModel: name, "loaded": false})
}

func (t *tools) deleteModel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	unwire := req.GetBool(argUnwire, true)
	if err := t.svc.Delete(ctx, name, unwire); err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{argModel: name, "deleted": true, "unwired": unwire})
}

func (t *tools) wire(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	ref, err := t.svc.Wire(ctx, name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{argModel: name, "modelConfig": ref})
}

func (t *tools) unwire(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argModel)
	if err != nil {
		return errResult(err), nil
	}
	if err := t.svc.Unwire(ctx, name); err != nil {
		return errResult(err), nil
	}
	return jsonResult(map[string]any{argModel: name, "modelConfig": nil})
}

func (t *tools) listJobs(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(map[string]any{"jobs": t.svc.Jobs()})
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
