// Package backend defines the serving-backend abstraction behind model-manager's
// API. One API — inventory of downloaded and loaded models, import with
// progress, load/unload, delete, wire-to-agents — implemented by
// per-installation drivers: `ollama` (host Ollama, the agentlab dev loop) and
// `kserve` (InferenceServices, HF cache inventory per node, download Jobs,
// presets). Drivers report what they support as explicit capability flags so
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
	// NodeInventory reports the download cache per node (kserve).
	NodeInventory bool `json:"nodeInventory"`
	// Search proxies a model hub search (kserve: Hugging Face Hub).
	Search bool `json:"search"`
}

// Info is the backend identity and health as reported by the driver.
type Info struct {
	Backend  Name   `json:"backend"`
	Version  string `json:"version,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Healthy  bool   `json:"healthy"`
	Message  string `json:"message,omitempty"`
}

// Model is a downloaded model in the backend's inventory.
type Model struct {
	// Name is the backend's canonical reference (e.g. "smollm2:135m",
	// "hf.co/org/repo:Q4_K_M", "org/repo" for an HF cache entry).
	Name          string    `json:"name"`
	Digest        string    `json:"digest,omitempty"`
	SizeBytes     int64     `json:"sizeBytes"`
	ModifiedAt    time.Time `json:"modifiedAt,omitempty"`
	Format        string    `json:"format,omitempty"`
	Family        string    `json:"family,omitempty"`
	ParameterSize string    `json:"parameterSize,omitempty"`
	Quantization  string    `json:"quantization,omitempty"`
	ContextLength int64     `json:"contextLength,omitempty"`
	// Capabilities are model features as reported by the backend
	// (e.g. completion, tools, vision, embedding, thinking).
	Capabilities []string `json:"capabilities,omitempty"`
	// Node is the node holding this cache entry (kserve per-node inventory).
	Node string `json:"node,omitempty"`
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
	// Status is a backend-specific state (kserve: Ready/Pending/...).
	Status string `json:"status,omitempty"`
}

// Progress is a pull-progress sample.
type Progress struct {
	Status         string `json:"status"`
	BytesCompleted int64  `json:"bytesCompleted"`
	BytesTotal     int64  `json:"bytesTotal"`
}

// PullRequest asks the backend to import a model.
type PullRequest struct {
	// Ref is the model reference in the backend's namespace.
	Ref string `json:"ref"`
}

// LoadRequest asks the backend to load / start serving a model.
type LoadRequest struct {
	Name string `json:"name"`
	// KeepAlive is how long the model stays loaded after the last request
	// (ollama: duration string or "-1" for forever).
	KeepAlive string `json:"keepAlive,omitempty"`
	// Preset selects a curated serving preset (kserve).
	Preset string `json:"preset,omitempty"`
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
