package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Options carries per-driver configuration. Each driver reads its own block.
type Options struct {
	Ollama   OllamaOptions
	KServe   KServeOptions
	Lemonade LemonadeOptions
	LMStudio LMStudioOptions
}

// KServeOptions configures the kserve driver. Empty strings mean "take it
// from the platform's discovery ConfigMap" (kind ModelServingConfig) where
// one exists; explicit values win over discovery.
type KServeOptions struct {
	// Dynamic and Clientset are the Kubernetes clients (required): the
	// ServiceAccount's, used for everything that runs without a caller.
	Dynamic   dynamic.Interface
	Clientset kubernetes.Interface
	// ClientsFor, when set, returns the clients a call should use for ctx —
	// the caller's own (downstream OAuth: the request carries the caller's
	// IdP token) or the ServiceAccount's. Nil always uses Dynamic/Clientset.
	ClientsFor func(ctx context.Context) (kubernetes.Interface, dynamic.Interface)

	// DiscoveryNamespace / DiscoveryConfigMap locate the ModelServingConfig
	// ConfigMap (default: model-manager's own namespace,
	// agent-platform-model-serving).
	DiscoveryNamespace string
	DiscoveryConfigMap string
	// DiscoveryTTL bounds how long the discovery document is cached.
	DiscoveryTTL time.Duration

	// Namespace is the serving namespace (LLMInferenceServices, Jobs, cache).
	Namespace string
	// GPUResourceName is the accelerator resource (nvidia.com/gpu).
	GPUResourceName string
	// CacheClaim / CacheMountPath describe the HF cache claim in Namespace.
	CacheClaim     string
	CacheMountPath string
	// CacheNodes overrides the nodes that hold the cache (default: derived
	// from the bound PersistentVolume's node affinity).
	CacheNodes []string
	// CacheIndexConfigMap names the ConfigMap in Namespace that remembers
	// which repository filled which cache directory (default
	// model-manager-cache-index).
	CacheIndexConfigMap string
	// Target is the cluster the backend acts on (a registered document names
	// it; empty: the local cluster, reported without a target).
	Target Target
	// PresetNamespace / PresetSelector locate the ServingPreset ConfigMaps.
	PresetNamespace string
	PresetSelector  string
	// GPUPool overrides the discovery ConfigMap's spec.gpuPool — the pool
	// taint to tolerate (when Taint is set) and the pool label to select
	// (when NodeSelector is non-empty) on scan pods, download Jobs and
	// composed predictors. A registered document's spec.kserve.gpuPool
	// lands here.
	GPUPool GPUPool
	// Router is the router shape of the composed LLMInferenceServices: the
	// route alone by default, the llm-d endpoint picker beside it when
	// Scheduler is set. A registered document's spec.kserve.router lands
	// here; a preset's spec.router.scheduler overrides it for that preset.
	Router Router

	// HFEndpoint is the Hugging Face Hub base URL.
	HFEndpoint string
	// HFTokenSecret / HFTokenSecretKey name a Secret in Namespace holding a
	// hub token (optional; gated repositories).
	HFTokenSecret    string
	HFTokenSecretKey string
	// HFTimeout bounds the hub lookups of one fit check (repository metadata,
	// file tree, safetensors index) or search: interactive calls that must
	// answer — or fall back to the preset's requirements — within a client's
	// meta-tool deadline, also when the hub does not answer at all (egress
	// blocked). Download Jobs are not affected.
	HFTimeout time.Duration

	// DownloadImage runs pre-warm downloads (default: the KServe
	// storage-initializer, so the cache holds exactly what an
	// LLMInferenceService would download).
	DownloadImage string
	// DownloadIgnorePatterns are passed as STORAGE_IGNORE_PATTERNS.
	DownloadIgnorePatterns []string
	// DownloadStallTimeout fails a download Job that wrote nothing to its
	// target directory for this long (the download script's stall clock)
	// with a reason naming the window and the bytes on disk, instead of
	// letting it hang.
	DownloadStallTimeout time.Duration
	// InitImage creates cache directories and scans the cache (a busybox-like
	// image with sh, find, stat, awk).
	InitImage string
	// JobTTL is ttlSecondsAfterFinished of download Jobs.
	JobTTL time.Duration
	// InventoryTTL bounds how long a cache scan is reused.
	InventoryTTL time.Duration
	// InventoryTimeout bounds one cache scan.
	InventoryTimeout time.Duration
	// InventoryMode selects how a node's cache is read: "pod" runs a
	// short-lived scan pod on the node; "daemonset" asks the cache-agent
	// pod (the chart's DaemonSet, `model-manager cache-agent`) running there.
	InventoryMode string
	// InventoryAgentSelector is the label selector of the cache-agent pods in
	// the serving namespace (daemonset mode).
	InventoryAgentSelector string
	// InventoryAgentPort is the cache-agent's listen port (daemonset mode).
	InventoryAgentPort int

	// BudgetSource picks the node memory budget: auto (GPU labels when
	// present, else allocatable memory), gpu-labels, allocatable.
	BudgetSource string
	// DefaultOverheadGiB is the serving overhead when no preset says.
	DefaultOverheadGiB float64
	// ReadyTimeout bounds WaitReady.
	ReadyTimeout time.Duration
	// ScaleUpTimeout is the GPU pool's scale-up budget: how long a predictor
	// may wait for a node while Karpenter refuses to launch one
	// (InsufficientInstanceCapacity) before its scheduling step, and the
	// phase, fail naming the refusal. Counted from the pod's creation; 0
	// never fails it.
	ScaleUpTimeout time.Duration
	// PollInterval is the readiness / job progress poll period.
	PollInterval time.Duration
}

// OllamaOptions configures the ollama driver.
type OllamaOptions struct {
	// Endpoint is the Ollama API base URL as reached by model-manager.
	Endpoint string
	// AgentHost is the Ollama host written into kagent ModelConfigs (as
	// reached by agent pods). Defaults to Endpoint.
	AgentHost string
	// Timeout bounds non-streaming API calls.
	Timeout time.Duration
	// MemoryBudgetGiB is the operator's memory budget for the proxied host in
	// GiB (decimals allowed), reported on the host node instead of MemTotal of
	// the pod's /proc/meminfo — for installs where the pod's view is not the
	// host's. Empty or 0: none. Parsed by the driver so that an unusable value
	// is reported on the node rather than dropped.
	MemoryBudgetGiB string
}

// LemonadeOptions configures the lemonade driver.
type LemonadeOptions struct {
	// Endpoint is the Lemonade Server base URL as reached by model-manager
	// (its API lives under /api/v1).
	Endpoint string
	// AgentHost is the Lemonade Server base URL as reached by agent pods; the
	// driver appends /api/v1 and writes it into kagent ModelConfigs as the
	// OpenAI-compatible baseUrl. Defaults to Endpoint.
	AgentHost string
	// Timeout bounds non-streaming API calls.
	Timeout time.Duration
	// LoadTimeout bounds a load: Lemonade starts the backend process and reads
	// the weights before it answers (0: 10 minutes).
	LoadTimeout time.Duration
}

// LMStudioOptions configures the lmstudio driver.
type LMStudioOptions struct {
	// Endpoint is the LM Studio base URL as reached by model-manager (its
	// API lives under /api/v1).
	Endpoint string
	// AgentHost is the LM Studio base URL as reached by agent pods; the
	// driver appends /v1 and writes it into kagent ModelConfigs as the
	// OpenAI-compatible baseUrl. Defaults to Endpoint.
	AgentHost string
	// Timeout bounds non-streaming API calls.
	Timeout time.Duration
	// LoadTimeout bounds a load: LM Studio reads the weights before it
	// answers (0: 10 minutes).
	LoadTimeout time.Duration
}

// Factory builds a Backend from Options.
type Factory func(opts Options) (Backend, error)

var (
	registryMu sync.RWMutex
	registry   = map[Name]Factory{}
)

// Register makes a driver available to New. Drivers are registered
// explicitly by the command wiring, not via init side effects.
func Register(name Name, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// New builds the named driver.
func New(name Name, opts Options) (Backend, error) {
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return f(opts)
}

// Names lists the registered drivers, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, string(n))
	}
	sort.Strings(names)
	return names
}
