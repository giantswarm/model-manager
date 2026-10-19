package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/gitops"
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
	argPlacement         = "placement"
	argNodes             = "nodes"
	argTracing           = "tracing"
	argQuery             = "query"
	argLimit             = "limit"
)

// The commit target of a write in mode commit where Flux provenance names none.
const (
	argRepository = "repository"
	argBranch     = "branch"
	argPath       = "path"
)

// commitArgs are the mode argument of a write that commits and its explicit
// target.
func commitArgs() mcp.ToolOption {
	opts := []mcp.ToolOption{
		mcp.WithString(argMode, mcp.Enum(ModeApply, ModeCommit), mcp.Description("apply (default): write live as you (gitops_owned where Flux applies it from git). commit: open a pull request as you in its GitOps repository")),
		mcp.WithString(argRepository, mcp.Description("mode commit: the repository (owner/name) if no Flux provenance names one; branch and path with it override that (remote kserve: serving object only)")),
		mcp.WithString(argBranch, mcp.Description("mode commit: the base branch of repository")),
		mcp.WithString(argPath, mcp.Description("mode commit: the directory in repository a Flux Kustomization builds from (default: the root); model-manager's files go under <path>/model-manager/")),
	}
	return func(t *mcp.Tool) {
		for _, o := range opts {
			o(t)
		}
	}
}

// Why pull_model and delete_model never commit.
const (
	noCommitPull   = "a download is no object of a GitOps repository; wire the model with wire_model mode commit"
	noCommitDelete = "the weights are no object of a GitOps repository; remove a committed ModelConfig with unwire_model mode commit, then delete with unwire=false"
)

// applyOnly is the mode argument of a write that never commits: commit
// answers unsupported, its refusal (refuseCommit) naming what to do instead.
func applyOnly() mcp.ToolOption {
	return mcp.WithString(argMode, mcp.Enum(ModeApply, ModeCommit), mcp.Description("apply only: mode commit answers unsupported, naming what to do instead"))
}

// refuseCommit is applyOnly's refusal.
func refuseCommit(req mcp.CallToolRequest, tool, why string) error {
	mode, err := modeOf(req)
	if err != nil {
		return err
	}
	if mode == ModeCommit {
		return fmt.Errorf("%w: mode commit on %s: %s", backend.ErrUnsupported, tool, why)
	}
	return nil
}

// commitTarget is the explicit target of a request.
func commitTarget(req mcp.CallToolRequest) gitops.Target {
	return gitops.Target{Repository: req.GetString(argRepository, ""), Branch: req.GetString(argBranch, ""), Path: req.GetString(argPath, "")}
}

// modeOf is the request's mode, refused when it is neither.
func modeOf(req mcp.CallToolRequest) (string, error) {
	switch mode := req.GetString(argMode, ModeApply); mode {
	case ModeApply, ModeCommit:
		return mode, nil
	default:
		return "", fmt.Errorf("%w: mode %q: apply or commit", backend.ErrInvalid, mode)
	}
}

// dryRunArg is the dryRun argument of a write tool; returns says what the
// dry run answers.
func dryRunArg(returns string) mcp.ToolOption {
	return mcp.WithBoolean(argDryRun, mcp.Description("Write nothing; return the plan: "+returns))
}

// backendArg is the optional backend argument every tool takes.
func backendArg(what string) mcp.ToolOption {
	return mcp.WithString(argBackend, mcp.Description("Backend "+what+", as list_backends names it; optional with one backend"))
}

// readOnly annotates a tool that changes nothing; mcp-go defaults every tool
// to destructive and not idempotent.
func readOnly() mcp.ToolOption {
	return func(t *mcp.Tool) {
		mcp.WithReadOnlyHintAnnotation(true)(t)
		mcp.WithDestructiveHintAnnotation(false)(t)
		mcp.WithIdempotentHintAnnotation(true)(t)
	}
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
	// BackendsReason says why Backends is empty — the no_backend wording: no
	// backend is registered on this instance, the namespace it watches for
	// backend documents and how to register one. Absent while a backend is
	// configured, so an empty list is never left for the caller to guess at.
	BackendsReason string `json:"backendsReason,omitempty"`
	// Wiring describes where ModelConfigs are created; absent when agent
	// wiring is disabled.
	Wiring *service.WiringInfo `json:"wiring,omitempty"`
}

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
// build is what get_info and the MCP server identity report as the version.
func NewMCPServer(svc *service.Service, build buildinfo.Info, opts ...Option) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("model-manager", build.Version, append(tracingOptions(),
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the models one or several serving backends (ollama, kserve, lemonade, lmstudio) hold: list downloaded and loaded models, pull with progress, load/unload, delete, and wire models into kagent ModelConfigs so agents can use them. Backends are registered at runtime with add_backend (remove_backend drops one); an installation may run none yet: list_backends and get_info then answer an empty list with backendsReason saying that no backend is registered and how to register one. Call list_backends first to learn which backends this installation runs and which capabilities each supports; every model carries its backend, and every tool takes an optional backend argument — required when the same model reference exists on several backends (the tool then answers conflict). On kserve also use list_presets, search_models, check_fit and list_nodes before pulling or loading."),
	)...)
	t := &tools{svc: svc, build: build}
	for _, o := range opts {
		o(t)
	}
	t.registerBackendTools(s)

	s.AddTool(mcp.NewTool(ToolGetInfo,
		tooldesc.Describe("Report this server's build, its tool names, the configured backends and, with agent wiring on, where ModelConfigs go.", ""),
		readOnly(),
	), t.getInfo)

	s.AddTool(mcp.NewTool(ToolGetBackend,
		tooldesc.Describe("Report one serving backend (default: the first configured): health, version, load semantics and capability flags.",
			"Clients must honor the capability flags."),
		backendArg("to describe"),
		readOnly(),
	), t.getBackend)

	s.AddTool(mcp.NewTool(ToolListBackends,
		tooldesc.Describe("List the serving backends this model-manager runs, in order (the first is the default), with health and capabilities.",
			"Call it first: most tools take an optional backend. An empty list comes with backendsReason: no backend is registered and how to register one. "+
				"A ModelConfig labelled with a backend not listed is left over from one that went."),
		readOnly(),
	), t.listBackends)

	s.AddTool(mcp.NewTool(ToolListModels,
		tooldesc.Describe("List downloaded models with backend, size, family and parameters, whether each is loaded, and its kagent ModelConfig.",
			"Without backend every backend is listed; one that fails to answer is reported under errors while the others' models are returned."),
		backendArg("to list"),
		readOnly(),
	), t.listModels)

	s.AddTool(mcp.NewTool(ToolGetModel,
		tooldesc.Describe("Get one downloaded model with its loaded state and kagent ModelConfig.", ""),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference, e.g. smollm2:135m or hf.co/org/repo:Q4_K_M")),
		backendArg("holding the model; else resolved across backends (conflict when several hold it)"),
		readOnly(),
	), t.getModel)

	s.AddTool(mcp.NewTool(ToolListLoadedModels,
		tooldesc.Describe("List the models loaded or serving now, with memory use and expiry; on kserve phase, steps, API interfaces and endpoint.",
			"Not read-only on kserve: a served model model-manager manages that has no ModelConfig is wired by this read, as you (wiring.reason \"wired on read\"). "+
				"A Ready kserve model carries runtime and interfaces, the APIs its server registered ([] with interfacesReason when unreadable); "+
				"on the platform's LLM endpoint publicName is the model name clients send there. "+
				"gitops names the Flux object applying a serving object from git: change it there."),
		backendArg("to list"),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	), t.listLoaded)

	s.AddTool(mcp.NewTool(ToolPullModel,
		tooldesc.Describe("Download a model onto a backend: start a pull job and return at once; poll get_job for progress.",
			"The reference is an Ollama tag or hf.co/… GGUF (ollama), a Lemonade catalog name such as Qwen3-0.6B-GGUF (lemonade), "+
				"an LM Studio hub reference such as ibm/granite-4-micro (lmstudio), or a Hugging Face repository owner/name "+
				"(kserve: a download Job into a node's cache after a fit check). "+
				"On ollama, lemonade and lmstudio the model is wired into kagent on success unless wire=false; on kserve it is wired when served."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference to pull")),
		backendArg("to pull on; default: the first configured"),
		mcp.WithBoolean(argWire, mcp.Description("Create a kagent ModelConfig when the pull completes (ollama, lemonade, lmstudio; default: the server's autoWire setting)")),
		mcp.WithString(argPreset, mcp.Description("kserve: the serving preset the download is for, whose serving object mounts the cache; default: the one preset serving the model")),
		mcp.WithString(argNode, mcp.Description("kserve: node whose cache receives the download; default: the cache node or the node with the largest budget")),
		dryRunArg("whether the model is there, on kserve the fit verdict and target node, and the ModelConfig it would wire"),
		applyOnly(),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	), t.pull)

	s.AddTool(mcp.NewTool(ToolLoadModel,
		tooldesc.Describe("Load a model into memory, or on kserve serve it from a preset after a fit check; auto-wire writes its ModelConfig.",
			"kserve: refused with unavailable, nothing created, without the llm-d control plane, and refused before anything is created where no node (or GPU pool size) can host the preset. "+
				"With auto-wire on kserve the ModelConfig is written before the model is ready and a load job follows it to readiness. "+
				"A preset served already answers alreadyServing and servingNodes; placement copies on more nodes adds copies there, another node is conflict. mode commit is kserve only."),
		mcp.WithString(argModel, mcp.Description("Model reference (required unless preset is given)")),
		backendArg("holding the model; without it the model is resolved across backends"),
		mcp.WithString(argKeepAlive, mcp.Description("Keep the model loaded this long after the last request: ollama a duration (10m) or -1 forever; lemonade only -1 (pins it); lmstudio ignores it")),
		mcp.WithString(argPreset, mcp.Description("kserve: serving preset to compose the LLMInferenceService from; default: the single preset serving the model")),
		mcp.WithString(argNode, mcp.Description("kserve: pin the workload to this node")),
		mcp.WithString(argPlacement, mcp.Enum(backend.PlacementCopies, backend.PlacementSplit), mcp.Description("kserve: copies (default: one per node in nodes, else on node or the fit check's pick) or split: one model across one fast link's nodes")),
		mcp.WithArray(argNodes, mcp.WithStringItems(), mcp.Description("kserve: split, the nodes in rank order, the first serving the API (default: the first fast link hosting the model); copies, one copy on each")),
		mcp.WithBoolean(argTracing, mcp.Description("kserve: export the served model's traces under the caller's trace (default false: costs throughput); switching on a served preset answers conflict")),
		dryRunArg("the fit verdict, the serving object it would create or where it serves already, and the ModelConfig it would wire"),
		commitArgs(),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	), t.load)

	s.AddTool(mcp.NewTool(ToolListPresets,
		tooldesc.Describe("List the kserve serving presets, the only way to serve a model there: model, runtime, GPUs and memory requirements.",
			"A preset states no API interfaces; list_loaded_models reads them from the running server."),
		backendArg("to list presets of"),
		readOnly(),
	), t.listPresets)

	s.AddTool(mcp.NewTool(ToolSearchModels,
		tooldesc.Describe("Search the model hub (kserve: Hugging Face Hub) by free text; each hit with gated and private flags and its presets.",
			"Run check_fit before pulling."),
		mcp.WithString(argQuery, mcp.Required(), mcp.Description("Search text")),
		backendArg("whose hub to search"),
		mcp.WithNumber(argLimit, mcp.Description(fmt.Sprintf("Maximum results (default %d, max %d)", backend.SearchLimitDefault, backend.SearchLimitMax))),
		readOnly(),
	), t.search)

	s.AddTool(mcp.NewTool(ToolCheckFit,
		tooldesc.Describe("Check whether a model or serving preset fits a kserve node before pulling or loading: the verdict load_model reaches.",
			"Sizes the weights from the hub (an oci:// preset from its image), adds the serving overhead and vLLM's KV cache at --max-model-len, "+
				"and compares with the node's free budget less what served models reserve. "+
				"fits stands only on the checks made: skippedChecks names those it could not make, reason any estimate. "+
				"A GPU pool with no ready node is judged on its instance shapes (unverified without them). Says whether the model is cached there and needs a hub token."),
		mcp.WithString(argModel, mcp.Description("Hugging Face repository owner/name (required unless preset is given)")),
		backendArg("to check on; required when several backends offer fit checks"),
		mcp.WithString(argPreset, mcp.Description("Serving preset (overhead, model id)")),
		mcp.WithString(argNode, mcp.Description("Node to check against; default: the best eligible node")),
		mcp.WithString(argPlacement, mcp.Enum(backend.PlacementCopies, backend.PlacementSplit), mcp.Description("kserve: copies (default; each holds the whole model) or split across one fast link's nodes (each its share); the answer recommends one")),
		mcp.WithArray(argNodes, mcp.WithStringItems(), mcp.Description("kserve: split, the nodes to split across (default: the first fast link hosting the model); copies, one copy on each, fitting when every copy does")),
		readOnly(),
	), t.checkFit)

	s.AddTool(mcp.NewTool(ToolListNodes,
		tooldesc.Describe("List the nodes a backend serves on, with memory budget, what loaded models reserve and the download cache each holds.",
			"kserve: accelerator nodes only; eligible and eligibilityReason say whether a model can be served there, and load_model, pull_model and check_fit refuse an ineligible node with that reason. "+
				"modelImageEligible says the same for a preset served from an OCI model image. "+
				"ollama and lemonade: the proxied host, always eligible. lmstudio answers unsupported: LM Studio exposes no host hardware."),
		backendArg("to list nodes of"),
		readOnly(),
	), t.listNodes)

	s.AddTool(mcp.NewTool(ToolUnloadModel,
		tooldesc.Describe("Unload a model from memory, or on kserve stop serving it and unwire its ModelConfig; the download stays.",
			"On kserve only a ModelConfig this model-manager created is unwired; any other is named in modelConfigLeft. "+
				"next says what list_loaded_models shows meanwhile. mode commit is kserve only."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("holding the model; without it the model is resolved across backends"),
		dryRunArg("on kserve the serving objects it would delete and the ModelConfig to unwire, elsewhere whether it is loaded"),
		commitArgs(),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.unload)

	s.AddTool(mcp.NewTool(ToolDeleteModel,
		tooldesc.Describe("Delete a downloaded model and, by default, its kagent ModelConfig.",
			"lmstudio answers unsupported (LM Studio removes models only with `lms rm` on the host): check list_backends' delete capability, and use unwire_model there to drop only the ModelConfig."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("holding the model; without it the model is resolved across backends"),
		mcp.WithBoolean(argUnwire, mcp.Description("Also remove the ModelConfig (default true)")),
		dryRunArg("whether the model is loaded and the ModelConfig the delete would unwire"),
		applyOnly(),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.deleteModel)

	s.AddTool(mcp.NewTool(ToolWireModel,
		tooldesc.Describe("Create or refresh the kagent ModelConfig for a downloaded model so agents can use it.",
			"The backend decides how the ModelConfig authenticates (a kserve model on the models Gateway forwards the caller's token, a keyless in-cluster endpoint gets kagent's placeholder key) "+
				"unless apiKeyPassthrough or apiKeySecret says otherwise; the two are mutually exclusive. "+
				"A ModelConfig Flux applies from git is never written live: gitops_owned names the Flux object."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("holding the model; without it the model is resolved across backends"),
		mcp.WithBoolean(argAPIKeyPassthrough, mcp.Description("Forward the Bearer token of the agent's request as the API key, for an endpoint that admits the person's token (the models Gateway); no Secret")),
		mcp.WithString(argAPIKeySecret, mcp.Description("Name of an existing Secret in the kagent namespace holding a static API key the endpoint checks; model-manager never creates or deletes it")),
		mcp.WithString(argAPIKeySecretKey, mcp.Description("Key within apiKeySecret holding the API key (default OPENAI_API_KEY)")),
		dryRunArg("the manifests, the Flux object applying the ModelConfig from git, another owner's ModelConfig wiring it"),
		commitArgs(),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.wire)

	s.AddTool(mcp.NewTool(ToolUnwireModel,
		tooldesc.Describe("Delete the kagent ModelConfig model-manager created for a model, and its placeholder Secret; the model stays.",
			"Also removes a leftover without an instance label (written before model-manager recorded its creator) once no Agent, AgentTemplate or Harness references it; "+
				"referenced, it answers conflict naming them. A ModelConfig Flux applies from git is never deleted live: gitops_owned names the Flux object."),
		mcp.WithString(argModel, mcp.Required(), mcp.Description("Model reference")),
		backendArg("the ModelConfig belongs to; else found among the wired ones (conflict when ambiguous)"),
		dryRunArg("the ModelConfig and Secret it would delete and the Flux object applying the ModelConfig from git"),
		commitArgs(),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.unwire)

	s.AddTool(mcp.NewTool(ToolListJobs,
		tooldesc.Describe("List pull and load jobs, newest first, with backend, phase and progress.",
			"On kserve a pull job carries the node whose cache receives the download and the preset it is for."),
		backendArg("to list jobs of"),
		readOnly(),
	), t.listJobs)

	s.AddTool(mcp.NewTool(ToolGetJob,
		tooldesc.Describe("Get one job: backend, phase, progress, error, and the ModelConfig created on success.", ""),
		mcp.WithString(argJobID, mcp.Required(), mcp.Description("Job id from pull_model")),
		readOnly(),
	), t.getJob)

	s.AddTool(mcp.NewTool(ToolCancelJob,
		tooldesc.Describe("Cancel a running pull or load job; cancelling a load leaves the model serving (unload_model stops it).", ""),
		mcp.WithString(argJobID, mcp.Required(), mcp.Description("Job id")),
		dryRunArg("the job it would cancel"),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	), t.cancelJob)

	return s
}

type tools struct {
	svc   *service.Service
	build buildinfo.Info
	store *registry.Store // nil: no Kubernetes access, registration tools refuse
}

func (t *tools) getInfo(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info := Info{Version: t.build.Version, Commit: t.build.Commit, Built: t.build.Date, Tools: ToolNames(), Backends: t.svc.Names(), Wiring: t.svc.Wiring()}
	if len(info.Backends) == 0 {
		info.Backends = []backend.Name{}
		info.BackendsReason = t.svc.NoBackendReason()
	}
	return jsonResult(info)
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
	backends, invalid := t.svc.Backends(ctx)
	out := backendsBody(t.svc, backends)
	if len(invalid) > 0 {
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
	if err := refuseCommit(req, "pull_model", noCommitPull); err != nil {
		return errResult(err), nil
	}
	opts := service.PullOptions{Backend: req.GetString(argBackend, ""), Model: name, Wire: wire, Preset: req.GetString(argPreset, ""), Node: req.GetString(argNode, "")}
	if req.GetBool(argDryRun, false) {
		plan, err := t.svc.PlanPull(ctx, opts)
		return planResult(plan, err)
	}
	job, created, err := t.svc.Pull(ctx, opts)
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
	opts := service.LoadOptions{Backend: req.GetString(argBackend, ""), Model: name, KeepAlive: req.GetString(argKeepAlive, ""), Preset: preset, Node: req.GetString(argNode, ""), Placement: req.GetString(argPlacement, ""), Nodes: req.GetStringSlice(argNodes, nil), Tracing: req.GetBool(argTracing, false)}
	mode, err := modeOf(req)
	switch {
	case err != nil:
		return errResult(err), nil
	case mode == ModeCommit:
		plan, err := t.svc.CommitLoad(ctx, opts, commitTarget(req), req.GetBool(argDryRun, false))
		return commitPlanResult(plan, err, req.GetBool(argDryRun, false))
	case req.GetBool(argDryRun, false):
		plan, err := t.svc.PlanLoad(ctx, opts)
		return planResult(plan, err)
	}
	m, err := t.svc.Load(ctx, opts)
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
	res, err := t.svc.FitCheck(ctx, req.GetString(argBackend, ""), backend.FitRequest{Model: req.GetString(argModel, ""), Preset: req.GetString(argPreset, ""), Node: req.GetString(argNode, ""), Placement: req.GetString(argPlacement, ""), Nodes: req.GetStringSlice(argNodes, nil)})
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
	mode, err := modeOf(req)
	switch {
	case err != nil:
		return errResult(err), nil
	case mode == ModeCommit:
		plan, err := t.svc.CommitUnload(ctx, req.GetString(argBackend, ""), name, commitTarget(req), req.GetBool(argDryRun, false))
		return commitPlanResult(plan, err, req.GetBool(argDryRun, false))
	case req.GetBool(argDryRun, false):
		plan, err := t.svc.PlanUnload(ctx, req.GetString(argBackend, ""), name)
		return planResult(plan, err)
	}
	view, err := t.svc.Unload(ctx, req.GetString(argBackend, ""), name)
	if err != nil {
		return errResult(err), nil
	}
	out := map[string]any{argBackend: view.Backend, argModel: name, "loaded": false}
	if view.Backend.Kind() == backend.NameKServe {
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
	if err := refuseCommit(req, "delete_model", noCommitDelete); err != nil {
		return errResult(err), nil
	}
	if req.GetBool(argDryRun, false) {
		plan, err := t.svc.PlanDelete(ctx, req.GetString(argBackend, ""), name, unwire)
		return planResult(plan, err)
	}
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
	mode, err := modeOf(req)
	if err != nil {
		return errResult(err), nil
	}
	if mode == ModeCommit {
		view, err := t.svc.CommitWire(ctx, req.GetString(argBackend, ""), name, opts, commitTarget(req), req.GetBool(argDryRun, false))
		if err != nil {
			return errResult(err), nil
		}
		return jsonResult(commitView(view, req.GetBool(argDryRun, false)))
	}
	if req.GetBool(argDryRun, false) {
		plan, err := t.svc.PlanWire(ctx, req.GetString(argBackend, ""), name, opts)
		if err != nil {
			return errResult(err), nil
		}
		return jsonResult(dryRunView(plan))
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
	mode, err := modeOf(req)
	if err != nil {
		return errResult(err), nil
	}
	if mode == ModeCommit {
		view, err := t.svc.CommitUnwire(ctx, req.GetString(argBackend, ""), name, commitTarget(req), req.GetBool(argDryRun, false))
		if err != nil {
			return errResult(err), nil
		}
		return jsonResult(commitView(view, req.GetBool(argDryRun, false)))
	}
	if req.GetBool(argDryRun, false) {
		plan, err := t.svc.PlanUnwire(ctx, req.GetString(argBackend, ""), name)
		if err != nil {
			return errResult(err), nil
		}
		return jsonResult(dryRunView(plan))
	}
	u, err := t.svc.Unwire(ctx, req.GetString(argBackend, ""), name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(unwiredView(name, u))
}

// unwiredView is an applied unwire's answer: the mode that ran, the backend
// the ModelConfig belonged to and the ModelConfig deleted (name and
// namespace), null with removed false when nothing was wired.
func unwiredView(model string, u *service.Unwired) map[string]any {
	out := map[string]any{argModel: model, argMode: ModeApply, "modelConfig": u.ModelConfig, "removed": u.ModelConfig != nil}
	if u.Backend != "" {
		out[argBackend] = u.Backend
	}
	return out
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
	if req.GetBool(argDryRun, false) {
		job, err := t.svc.PlanCancel(id)
		if err != nil {
			return errResult(err), nil
		}
		return jsonResult(map[string]any{argDryRun: true, "job": job})
	}
	job, err := t.svc.CancelJob(id)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(job)
}

// commitView is a write's answer in mode commit: the plan and the pull
// request (commit), with next saying what follows.
func commitView(view *service.CommitView, dryRun bool) map[string]any {
	out := dryRunView(view.WirePlan)
	out[argDryRun], out[argMode] = dryRun, ModeCommit
	switch c := view.Commit; {
	case c == nil && view.AlreadyWired != nil:
		out["next"] = "the served model is wired already by " + view.AlreadyWired.Namespace + "/" + view.AlreadyWired.Name + ": nothing to commit"
	case c == nil:
		out["next"] = "nothing is wired: nothing to commit"
	case c.PullRequest == "" && !dryRun:
		out["commit"], out["next"] = c, c.Repository+" already carries the change: nothing to commit"
	case dryRun:
		out["commit"], out["next"] = c, "re-run without dryRun to open the pull request as "+c.Author+" in "+c.Repository
	default:
		out["commit"], out["next"] = c, "review and merge "+c.PullRequest+"; Flux applies it after the merge"
	}
	return out
}

// planResult is an operational write's dry-run answer.
func planResult(plan *service.OpPlan, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(struct {
		DryRun bool `json:"dryRun"`
		*service.OpPlan
	}{true, plan})
}

// commitPlanResult is a load or unload in mode commit: the plan, the pull
// request (commit) and what follows (next).
func commitPlanResult(plan *service.OpPlan, err error, dryRun bool) (*mcp.CallToolResult, error) {
	if err != nil {
		return errResult(err), nil
	}
	var next string
	switch c := plan.Commit; {
	case c == nil && plan.AlreadyServing:
		next = "the preset serves already: nothing to commit"
	case c == nil:
		next = "nothing to commit"
	case dryRun:
		next = "re-run without dryRun to open the pull request as " + c.Author + " in " + c.Repository
	case c.PullRequest == "":
		next = c.Repository + " already carries the change: nothing to commit"
	default:
		next = "review and merge " + c.PullRequest + "; Flux applies it after the merge"
	}
	return jsonResult(struct {
		DryRun bool   `json:"dryRun"`
		Mode   string `json:"mode"`
		*service.OpPlan
		Next string `json:"next"`
	}{dryRun, ModeCommit, plan, next})
}

// dryRunView is a write tool's dry-run answer: the plan, marked as one.
func dryRunView(plan *service.WirePlan) map[string]any {
	out := map[string]any{argDryRun: true, argMode: ModeApply, argBackend: plan.Backend, argModel: plan.Model, "manifests": plan.Manifests}
	if plan.ModelConfig != "" {
		out["modelConfig"] = plan.ModelConfig
	}
	if plan.Namespace != "" {
		out["namespace"] = plan.Namespace
	}
	if plan.GitOps != nil {
		out["gitops"] = plan.GitOps
	}
	if plan.Replaces != "" {
		out["replaces"] = plan.Replaces
	}
	if plan.AlreadyWired != nil {
		out["alreadyWired"] = plan.AlreadyWired
	}
	return out
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
