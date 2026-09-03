package backend

import (
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
	Ollama OllamaOptions
	KServe KServeOptions
}

// KServeOptions configures the kserve driver. Empty strings mean "take it
// from the platform's discovery ConfigMap" (kind ModelServingConfig) where
// one exists; explicit values win over discovery.
type KServeOptions struct {
	// Dynamic and Clientset are the Kubernetes clients (required).
	Dynamic   dynamic.Interface
	Clientset kubernetes.Interface

	// DiscoveryNamespace / DiscoveryConfigMap locate the ModelServingConfig
	// ConfigMap (default: model-manager's own namespace,
	// agent-platform-model-serving).
	DiscoveryNamespace string
	DiscoveryConfigMap string
	// DiscoveryTTL bounds how long the discovery document is cached.
	DiscoveryTTL time.Duration

	// Namespace is the serving namespace (InferenceServices, Jobs, cache).
	Namespace string
	// Runtime is the ClusterServingRuntime name a preset without one uses.
	Runtime string
	// GPUResourceName is the accelerator resource (nvidia.com/gpu).
	GPUResourceName string
	// CacheClaim / CacheMountPath describe the HF cache claim in Namespace.
	CacheClaim     string
	CacheMountPath string
	// CacheNodes overrides the nodes that hold the cache (default: derived
	// from the bound PersistentVolume's node affinity).
	CacheNodes []string
	// PresetNamespace / PresetSelector locate the ServingPreset ConfigMaps.
	PresetNamespace string
	PresetSelector  string

	// HFEndpoint is the Hugging Face Hub base URL.
	HFEndpoint string
	// HFTokenSecret / HFTokenSecretKey name a Secret in Namespace holding a
	// hub token (optional; gated repositories).
	HFTokenSecret    string
	HFTokenSecretKey string

	// DownloadImage runs pre-warm downloads (default: the KServe
	// storage-initializer, so the cache holds exactly what an
	// InferenceService would download).
	DownloadImage string
	// DownloadIgnorePatterns are passed as STORAGE_IGNORE_PATTERNS.
	DownloadIgnorePatterns []string
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
