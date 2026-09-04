// Package backend defines the serving-backend abstraction behind model-manager's
// API. One API — inventory of downloaded and loaded models, import with
// progress, load/unload, delete, wire-to-agents — implemented by
// per-installation drivers: `ollama` (host Ollama, the agentlab dev loop),
// `kserve` (InferenceServices, HF cache inventory per node, download Jobs,
// presets) and `lemonade` (a Lemonade Server — FastFlowLM on AMD Ryzen AI NPUs,
// llama.cpp on GPUs and CPUs — on laptops and workstations). Drivers report
// what they support as explicit capability flags so
// clients (portal, MCP) render per flag, never per backend name.
package backend

import (
	"context"
	"errors"
	"time"
)

// Name identifies a serving backend driver.
type Name string

const (
	// NameOllama is the host-Ollama driver (dev loop, laptop installs).
	NameOllama Name = "ollama"
	// NameKServe is the KServe/vLLM driver (GPU installs).
	NameKServe Name = "kserve"
	// NameLemonade is the Lemonade Server driver (AMD Ryzen AI hosts: FastFlowLM
	// on the NPU, llama.cpp on GPU and CPU).
	NameLemonade Name = "lemonade"
)

// Sentinel errors drivers return so the API layer can map them to status
// codes without knowing the driver.
var (
	// ErrNotFound means the referenced model does not exist on the backend.
	ErrNotFound = errors.New("model not found")
	// ErrUnsupported means the backend (or the deployment) does not offer the
	// operation; the matching capability flag is false.
	ErrUnsupported = errors.New("operation not supported")
	// ErrConflict means the operation would clobber state owned by someone else.
	ErrConflict = errors.New("conflict")
	// ErrInvalid means the request was malformed (empty reference, bad option).
	ErrInvalid = errors.New("invalid request")
	// ErrUnfit means the model does not fit the memory budget of any eligible
	// node (fit check refused a pull or a load); the message says why.
	ErrUnfit = errors.New("model does not fit")
)

// Capabilities are explicit data, not conditionals in clients. A flag is true
// only when the operation works on this deployment.
type Capabilities struct {
	// Pull imports a model by reference (registry tag, hf.co/... GGUF, HF repo).
	Pull bool `json:"pull"`
	// PullProgress means pull jobs report bytes completed/total.
	PullProgress bool `json:"pullProgress"`
	// Delete removes a downloaded model.
	Delete bool `json:"delete"`
	// Load starts serving / loads a downloaded model into memory.
	Load bool `json:"load"`
	// Unload stops serving / evicts a model from memory.
	Unload bool `json:"unload"`
	// LoadedModels lists the models currently loaded/serving with memory use.
	LoadedModels bool `json:"loadedModels"`
	// Wire creates kagent ModelConfigs for models. Set by the service from its
	// Kubernetes access, not by the driver.
	Wire bool `json:"wire"`
	// Presets offers curated serving presets (kserve).
	Presets bool `json:"presets"`
	// FitCheck validates a model against node memory before download/serve (kserve).
	FitCheck bool `json:"fitCheck"`
	// NodeInventory reports nodes with their memory budget and what loaded
	// models reserve (kserve: the accelerator nodes with their download cache
	// and whether each is a serving target; ollama: the proxied host, budget
	// from /proc/meminfo; lemonade: the proxied host as Lemonade's system-info
	// reports it — memory, accelerators, model store).
	NodeInventory bool `json:"nodeInventory"`
	// Search proxies a model hub search (kserve: Hugging Face Hub).
	Search bool `json:"search"`
}

// Keep-alive scopes a driver reports in Loading.KeepAliveScope.
const (
	// KeepAliveScopeRequest means every inference request re-arms the
	// eviction timer with its own keep-alive — or the server's default when
	// it sends none — so a Load only pre-warms (ollama).
	KeepAliveScopeRequest = "request"
	// KeepAliveScopeServer means the eviction timer is fixed server-side and
	// requests do not change it.
	KeepAliveScopeServer = "server"
)

// Loading describes how the backend brings models into memory and lets them
// go. Clients need it to explain a not-loaded model — "loads on the first
// request" versus "not serving, agents will fail" — without keying off the
// backend name.
type Loading struct {
	// OnDemand is true when the backend loads a model on the first inference
	// request naming it (ollama, lemonade): agents work on a not-loaded model, the first
	// turn pays the cold start. False means a model must be loaded / served
	// explicitly before agents can use it (kserve).
	OnDemand bool `json:"onDemand"`
	// IdleEviction is true when the backend evicts idle models on its own
	// (ollama keep-alive). False means a loaded model stays until unloaded
	// (kserve; lemonade also lets another model of the same type take the
	// slot, but never for being idle).
	IdleEviction bool `json:"idleEviction"`
	// KeepAliveDefault is model-manager's default keep-alive for a Load
	// request that carries none (ollama, the configured --default-keep-alive).
	// It is NOT the backend's own default for inference traffic (Ollama's
	// OLLAMA_KEEP_ALIVE), which the API cannot observe. Empty when the
	// backend has no keep-alive.
	KeepAliveDefault string `json:"keepAliveDefault,omitempty"`
	// KeepAliveScope says whose keep-alive wins: KeepAliveScopeRequest or
	// KeepAliveScopeServer. Empty when the backend has no keep-alive.
	KeepAliveScope string `json:"keepAliveScope,omitempty"`
}

// Info is the backend identity and health as reported by the driver.
type Info struct {
	Backend Name   `json:"backend"`
	Version string `json:"version,omitempty"`
	// Endpoint is the backend as reached by model-manager.
	Endpoint string `json:"endpoint,omitempty"`
	// AgentEndpoint is the backend as reached by agent pods — the host the
	// driver writes into ModelConfigs (ollama: the agent host, which may
	// differ from Endpoint; lemonade: the agent host plus /api/v1, the
	// OpenAI-compatible base URL). Empty when the backend has no single agent-facing
	// endpoint (kserve: every served model has its own predictor URL).
	// Clients that match ModelConfigs to models by hostname compare against
	// this, not Endpoint.
	AgentEndpoint string `json:"agentEndpoint,omitempty"`
	Healthy       bool   `json:"healthy"`
	Message       string `json:"message,omitempty"`
	// Loading is how the backend loads and evicts models; always reported,
	// health does not change it.
	Loading Loading `json:"loading"`
}

// Model is a downloaded model in the backend's inventory.
type Model struct {
	// Name is the backend's canonical reference (e.g. "smollm2:135m",
	// "hf.co/org/repo:Q4_K_M", "org/repo" for an HF cache entry).
	Name          string    `json:"name"`
	Digest        string    `json:"digest,omitempty"`
	SizeBytes     int64     `json:"sizeBytes"`
	ModifiedAt    time.Time `json:"modifiedAt,omitzero"`
	Format        string    `json:"format,omitempty"`
	Family        string    `json:"family,omitempty"`
	ParameterSize string    `json:"parameterSize,omitempty"`
	Quantization  string    `json:"quantization,omitempty"`
	ContextLength int64     `json:"contextLength,omitempty"`
	// Runtime is what the backend runs the model with, on backends that have
	// several (lemonade: the recipe — flm for FastFlowLM on the NPU, llamacpp,
	// ryzenai-llm, ...). Empty on a backend with one runtime (ollama) or where
	// the preset says (kserve).
	Runtime string `json:"runtime,omitempty"`
	// Capabilities are model features as reported by the backend
	// (e.g. completion, tools, vision, embedding, thinking).
	Capabilities []string `json:"capabilities,omitempty"`
	// Node is the node holding this cache entry (kserve per-node inventory).
	Node string `json:"node,omitempty"`
	// Downloaded is set by backends that also list models which are not (yet)
	// in their cache: false for a model known only from a serving preset or a
	// running InferenceService whose weights are not cached. Absent means the
	// backend lists downloads only (ollama).
	Downloaded *bool `json:"downloaded,omitempty"`
	// Path is the cache directory holding the files, relative to the cache
	// mount (kserve: the InferenceService name the storage-initializer uses).
	Path string `json:"path,omitempty"`
	// Preset is the serving preset whose model this is, when one matches
	// (kserve).
	Preset string `json:"preset,omitempty"`
}

// LoadedModel is a model currently loaded in memory / serving.
type LoadedModel struct {
	Name          string     `json:"name"`
	Digest        string     `json:"digest,omitempty"`
	SizeBytes     int64      `json:"sizeBytes"`
	VRAMBytes     int64      `json:"vramBytes,omitempty"`
	ContextLength int64      `json:"contextLength,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	// Endpoint is where inference is served (kserve: InferenceService URL).
	Endpoint string `json:"endpoint,omitempty"`
	Node     string `json:"node,omitempty"`
	// Status is a backend-specific state: "loaded" (ollama); "Ready",
	// "NotReady" or "Pending" (kserve InferenceService readiness).
	Status string `json:"status,omitempty"`
	// Message explains a non-ready Status (kserve: the Ready condition
	// message or modelStatus failure).
	Message string `json:"message,omitempty"`
	// Resource is the serving object behind this entry (kserve: the
	// InferenceService name, which is also the served model name).
	Resource string `json:"resource,omitempty"`
	// Preset is the serving preset the entry was created from (kserve).
	Preset string `json:"preset,omitempty"`
	// GPUs is the accelerator count the predictor requests (kserve).
	GPUs int64 `json:"gpus,omitempty"`
	// ManagedBy is the app.kubernetes.io/managed-by label of the serving
	// object (kserve: model-manager, backstage, ...; empty when unlabelled).
	ManagedBy string `json:"managedBy,omitempty"`
	// Device is where the model runs as the backend reports it (lemonade:
	// npu, gpu, cpu, or several such as "gpu npu"). Absent when the backend
	// does not say (ollama: VRAMBytes tells; kserve: GPUs).
	Device string `json:"device,omitempty"`
	// Pinned is true when the model is exempt from the backend's slot
	// eviction (lemonade: loaded with keepAlive -1).
	Pinned bool `json:"pinned,omitempty"`
}

// Progress is a pull-progress sample.
type Progress struct {
	Status         string `json:"status"`
	BytesCompleted int64  `json:"bytesCompleted"`
	BytesTotal     int64  `json:"bytesTotal"`
	// Node and Preset name where the pull lands once the backend has decided
	// (kserve: the node the fit check picked when the request left it open,
	// the preset resolved from the model). Empty leaves the job's values as
	// they are; drivers without placement (ollama) never set them.
	Node   string `json:"node,omitempty"`
	Preset string `json:"preset,omitempty"`
}

// PullRequest asks the backend to import a model.
type PullRequest struct {
	// Ref is the model reference in the backend's namespace.
	Ref string `json:"ref"`
	// Preset names the serving preset the download is for (kserve): the
	// weights land in the cache directory that preset's InferenceService
	// mounts. Empty picks the preset serving Ref when exactly one does.
	Preset string `json:"preset,omitempty"`
	// Node pins the download to one node's cache (kserve). Empty lets the
	// backend pick (the cache node, or the node with the largest budget).
	Node string `json:"node,omitempty"`
}

// LoadRequest asks the backend to load / start serving a model.
type LoadRequest struct {
	Name string `json:"name"`
	// KeepAlive is how long the model stays loaded after the last request
	// (ollama: duration string or "-1" for forever). lemonade has no timer:
	// "-1" pins the model against slot eviction, any other value is ignored.
	KeepAlive string `json:"keepAlive,omitempty"`
	// Preset selects a curated serving preset (kserve).
	Preset string `json:"preset,omitempty"`
	// Node pins the predictor to one node (kserve).
	Node string `json:"node,omitempty"`
}

// Preset is a curated serving recipe (kserve: a published ServingPreset).
type Preset struct {
	Name          string            `json:"name"`
	DisplayName   string            `json:"displayName"`
	Description   string            `json:"description,omitempty"`
	Source        string            `json:"source,omitempty"`
	Model         string            `json:"model"`
	StorageURI    string            `json:"storageUri,omitempty"`
	Format        string            `json:"format,omitempty"`
	Runtime       string            `json:"runtime,omitempty"`
	ContextLength int64             `json:"contextLength,omitempty"`
	Capabilities  []string          `json:"capabilities,omitempty"`
	License       string            `json:"license,omitempty"`
	GPUs          int64             `json:"gpus"`
	WeightsBytes  int64             `json:"weightsBytes"`
	OverheadBytes int64             `json:"overheadBytes"`
	RequiredBytes int64             `json:"requiredBytes"`
	Args          []string          `json:"args,omitempty"`
	NodeSelector  map[string]string `json:"nodeSelector,omitempty"`
	ChatTemplate  string            `json:"chatTemplate,omitempty"`
}

// SearchResult is one model-hub hit.
type SearchResult struct {
	ID           string    `json:"id"`
	Author       string    `json:"author,omitempty"`
	Downloads    int64     `json:"downloads"`
	Likes        int64     `json:"likes"`
	Gated        bool      `json:"gated"`
	Private      bool      `json:"private"`
	PipelineTag  string    `json:"pipelineTag,omitempty"`
	Library      string    `json:"library,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	LastModified time.Time `json:"lastModified,omitempty"`
	// Presets are the serving presets that serve this exact model.
	Presets []string `json:"presets,omitempty"`
}

// FitRequest asks whether a model can be served on a node.
type FitRequest struct {
	Model  string `json:"model"`
	Preset string `json:"preset,omitempty"`
	Node   string `json:"node,omitempty"`
}

// FitResult is the outcome of a fit check.
type FitResult struct {
	Model string `json:"model"`
	// Fits is true when RequiredBytes <= BudgetBytes on Node.
	Fits   bool   `json:"fits"`
	Reason string `json:"reason,omitempty"`
	// Preset is the preset the check used for overhead (and weights when the
	// hub could not tell); Presets lists every preset serving the model.
	Preset  string   `json:"preset,omitempty"`
	Presets []string `json:"presets,omitempty"`
	// WeightsBytes is the size of the weights as served; WeightsSource says
	// where the number came from (safetensors-index, tree, preset).
	WeightsBytes  int64  `json:"weightsBytes"`
	WeightsSource string `json:"weightsSource,omitempty"`
	OverheadBytes int64  `json:"overheadBytes"`
	RequiredBytes int64  `json:"requiredBytes"`
	// DownloadBytes is what a pull would fetch (all repository files).
	DownloadBytes int64 `json:"downloadBytes,omitempty"`
	// Node is the node the check was made against; BudgetSource says how its
	// budget was derived (gpu-labels, allocatable, annotation).
	Node          string `json:"node,omitempty"`
	BudgetBytes   int64  `json:"budgetBytes"`
	BudgetSource  string `json:"budgetSource,omitempty"`
	ReservedBytes int64  `json:"reservedBytes"`
	FreeBytes     int64  `json:"freeBytes"`
	// Gated / Private describe the hub repository; TokenConfigured says
	// whether a hub token is available for gated downloads.
	Gated           bool `json:"gated"`
	Private         bool `json:"private"`
	TokenConfigured bool `json:"tokenConfigured"`
	// Cached is true when the model is already in the node's cache.
	Cached bool `json:"cached"`
}

// NodeInfo is one node's serving budget and cache state.
type NodeInfo struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	Architecture string `json:"architecture,omitempty"`
	// Eligible is true when a model can be served on this node right now:
	// kserve — ready, inside the discovery node selector, and able to mount
	// the cache claim when predictors mount it; ollama — always, the host is
	// the only serving target. EligibilityReason explains a false value in
	// one human-readable sentence (several reasons joined with "; "); a
	// serve, pull or fit-check request naming the node fails with it.
	Eligible          bool   `json:"eligible"`
	EligibilityReason string `json:"eligibilityReason,omitempty"`
	// AllocatableMemoryBytes is the node's allocatable memory.
	AllocatableMemoryBytes int64 `json:"allocatableMemoryBytes"`
	GPUCount               int64 `json:"gpuCount"`
	// GPUMemoryBytes is the memory of one GPU from the node labels.
	GPUMemoryBytes int64  `json:"gpuMemoryBytes,omitempty"`
	GPUProduct     string `json:"gpuProduct,omitempty"`
	// Accelerated is set by backends that cannot enumerate accelerators but
	// see them in use (ollama): true when any loaded model has memory on one
	// (running.vramBytes > 0), false when none has or nothing is loaded.
	// Absent when the backend counts accelerators in GPUCount (kserve).
	Accelerated *bool `json:"accelerated,omitempty"`
	// BudgetBytes is the memory a served model may use on this node and
	// BudgetSource how it was derived: gpu-labels, allocatable, annotation
	// (the node's model-manager.giantswarm.io/memory-budget-gib annotation
	// overrode the configured source), host-meminfo (ollama: MemTotal of
	// /proc/meminfo as the model-manager pod sees it) or override (ollama:
	// the operator's ollama.memoryBudgetGiB replaced that figure) or
	// system-info (lemonade: the host memory Lemonade Server reports).
	BudgetBytes  int64  `json:"budgetBytes"`
	BudgetSource string `json:"budgetSource,omitempty"`
	// Message notes a budget derivation problem, e.g. an ignored, unparsable
	// budget annotation or override, or explains the figures (ollama).
	Message string `json:"message,omitempty"`
	// ReservedBytes is what the models already served on the node need.
	ReservedBytes int64 `json:"reservedBytes"`
	FreeBytes     int64 `json:"freeBytes"`
	// Cache describes the download cache on this node; nil when the node
	// holds no cache (always on ollama; lemonade: the model store).
	Cache *NodeCache `json:"cache,omitempty"`
}

// NodeCache is the download cache as seen from one node.
type NodeCache struct {
	Claim     string    `json:"claim"`
	MountPath string    `json:"mountPath"`
	Models    int       `json:"models"`
	BytesUsed int64     `json:"bytesUsed"`
	ScannedAt time.Time `json:"scannedAt,omitempty"`
	// Shared is true when the claim is not node-local (network storage): the
	// same contents are visible from every node.
	Shared bool `json:"shared,omitempty"`
	// Inventory says how the contents were read: "pod" (a short-lived scan
	// pod on the node), "daemonset" (the cache-agent DaemonSet pod there) or
	// "models" (lemonade: the backend's own model list, no scan).
	Inventory string `json:"inventory,omitempty"`
	// Error is the last inventory failure; the listed contents may be stale.
	Error string `json:"error,omitempty"`
}

// PresetLister is implemented by backends with Capabilities().Presets.
type PresetLister interface {
	ListPresets(ctx context.Context) ([]Preset, error)
}

// Searcher is implemented by backends with Capabilities().Search.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// FitChecker is implemented by backends with Capabilities().FitCheck.
type FitChecker interface {
	FitCheck(ctx context.Context, req FitRequest) (*FitResult, error)
}

// NodeLister is implemented by backends with Capabilities().NodeInventory.
type NodeLister interface {
	ListNodes(ctx context.Context) ([]NodeInfo, error)
}

// ServeLifecycle marks backends whose inference endpoint exists only while a
// model is loaded (kserve: the InferenceService). The service then wires the
// model into kagent when it becomes ready rather than right after Load,
// unwires it on Unload, and never wires after a pull (a cached model has no
// endpoint).
type ServeLifecycle interface {
	// WaitReady blocks until the served model answers or ctx ends.
	WaitReady(ctx context.Context, model string) error
}

// PullAdopter is implemented by backends whose pulls survive a restart of
// model-manager (kserve: download Jobs). The service re-registers them as
// jobs on start.
type PullAdopter interface {
	RunningPulls(ctx context.Context) ([]PullRequest, error)
}

// AgentEndpoint describes how kagent agents reach a model on this backend; the
// wiring layer turns it into a ModelConfig (plus placeholder secret when the
// provider needs one).
type AgentEndpoint struct {
	// Provider is the kagent ModelConfig provider ("Ollama", "OpenAI").
	Provider string `json:"provider"`
	// Host is the Ollama API host (provider Ollama).
	Host string `json:"host,omitempty"`
	// BaseURL is the OpenAI-compatible base URL (provider OpenAI).
	BaseURL string `json:"baseUrl,omitempty"`
	// Model is the model name passed to the provider.
	Model string `json:"model"`
	// PlaceholderAPIKey is true when the provider requires an API key the
	// endpoint does not check (keyless vLLM behind kagent's OpenAI provider).
	PlaceholderAPIKey bool `json:"placeholderApiKey,omitempty"`
	// Name is the ModelConfig name the backend wants (kserve: the
	// InferenceService name, the same rule the portal applies). Empty derives
	// the name from the model reference.
	Name string `json:"name,omitempty"`
}

// Backend is the driver contract. The kserve driver implements the same
// interface: ListModels is the per-node HF cache inventory, Pull is a download
// Job, Load/Unload create and delete InferenceServices, LoadRequest.Preset
// selects a serving preset.
type Backend interface {
	Name() Name
	Capabilities() Capabilities
	Info(ctx context.Context) Info
	ListModels(ctx context.Context) ([]Model, error)
	GetModel(ctx context.Context, name string) (*Model, error)
	ListLoaded(ctx context.Context) ([]LoadedModel, error)
	// Pull blocks until the import finished, calling progress as it goes.
	Pull(ctx context.Context, req PullRequest, progress func(Progress)) error
	Delete(ctx context.Context, name string) error
	Load(ctx context.Context, req LoadRequest) error
	Unload(ctx context.Context, name string) error
	AgentEndpoint(model string) AgentEndpoint
}
