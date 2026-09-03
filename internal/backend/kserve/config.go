package kserve

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/cacheagent"
)

// Defaults of the driver; every one of them can be overridden by options or,
// for the serving-layer values, by the platform's discovery ConfigMap.
const (
	DefaultDiscoveryConfigMap = "agent-platform-model-serving"
	DefaultNamespace          = "model-serving"
	DefaultRuntime            = "kserve-vllm"
	DefaultGPUResourceName    = "nvidia.com/gpu"
	DefaultCacheClaim         = "hf-cache"
	DefaultCacheMountPath     = "/mnt/models"
	// DefaultCacheIndexConfigMap is the ConfigMap in the serving namespace
	// that remembers which repository filled which cache directory
	// (index.go).
	DefaultCacheIndexConfigMap = "model-manager-cache-index"
	componentCacheIndex        = "cache-index"
	DefaultPresetSelector      = "agent-platform.giantswarm.io/serving-preset=true"
	DefaultHFEndpoint          = "https://huggingface.co"
	DefaultHFTokenSecretKey    = "token"
	// DefaultDownloadImage is the KServe storage-initializer: a pre-warm
	// download then produces exactly the files an InferenceService's own
	// download would, so a later start finds them and skips the download.
	DefaultDownloadImage = "docker.io/kserve/storage-initializer:v0.20.0"
	// DefaultInitImage creates cache directories and scans the cache.
	DefaultInitImage        = "gsoci.azurecr.io/giantswarm/alpine:3.22.1"
	DefaultBudgetSource     = "auto"
	DefaultOverheadGiB      = 30
	DefaultDiscoveryTTL     = time.Minute
	DefaultInventoryTTL     = 2 * time.Minute
	DefaultInventoryTimeout = 2 * time.Minute
	// InventoryModePod (default) scans a node's cache with a short-lived pod;
	// InventoryModeDaemonSet reads it from the cache-agent DaemonSet pod on
	// the node (chart value kserve.inventory.mode).
	InventoryModePod       = "pod"
	InventoryModeDaemonSet = "daemonset"
	// DefaultInventoryAgentSelector finds the cache-agent pods the chart's
	// DaemonSet runs (the chart adds the release instance label).
	DefaultInventoryAgentSelector = ComponentLabel + "=" + componentCacheAgent
	DefaultInventoryAgentPort     = cacheagent.DefaultPort
	componentCacheAgent           = "cache-agent"
	DefaultJobTTL                 = time.Hour
	DefaultReadyTimeout           = 2 * time.Hour
	DefaultPollInterval           = 5 * time.Second
	discoveryConfigKey            = "config.yaml"
	discoveryKind                 = "ModelServingConfig"
	presetConfigKey               = "preset.yaml"
	presetKind                    = "ServingPreset"
	agentPlatformAPIVersion       = "agent-platform.giantswarm.io/v1alpha1"
	budgetSourceAuto              = "auto"
	budgetSourceGPULabels         = "gpu-labels"
	budgetSourceAllocatable       = "allocatable"
	// budgetSourceAnnotation is reported when the node's BudgetAnnotation
	// overrode the configured source.
	budgetSourceAnnotation       = "annotation"
	gib                    int64 = 1 << 30
)

// discoveryDoc is the ModelServingConfig document the umbrella chart
// publishes (agent-platform-standalone, templates/model-serving/config.yaml).
type discoveryDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Namespace              string            `json:"namespace"`
		Runtime                string            `json:"runtime"`
		GPUResourceName        string            `json:"gpuResourceName"`
		RuntimeClassName       string            `json:"runtimeClassName"`
		NodeSelector           map[string]string `json:"nodeSelector"`
		DeploymentStrategyType string            `json:"deploymentStrategyType"`
		TimeoutSeconds         int64             `json:"timeoutSeconds"`
		Cache                  struct {
			Enabled        bool   `json:"enabled"`
			ClaimName      string `json:"claimName"`
			MountPath      string `json:"mountPath"`
			RedirectPolicy bool   `json:"redirectPolicy"`
		} `json:"cache"`
		Presets struct {
			Namespace     string   `json:"namespace"`
			LabelSelector string   `json:"labelSelector"`
			Names         []string `json:"names"`
		} `json:"presets"`
	} `json:"spec"`
}

// settings is the effective serving-layer configuration: discovery merged
// with explicit options (options win).
type settings struct {
	Namespace              string
	Runtime                string
	GPUResourceName        string
	RuntimeClassName       string
	NodeSelector           map[string]string
	DeploymentStrategyType string
	TimeoutSeconds         int64
	CacheEnabled           bool
	CacheClaim             string
	CacheMountPath         string
	CacheRedirectPolicy    bool
	PresetNamespace        string
	PresetSelector         string
	// DiscoveryFound reports whether the discovery ConfigMap was read.
	DiscoveryFound bool
	DiscoveryError string
}

// config resolves settings from options plus the discovery ConfigMap, cached
// for DiscoveryTTL so a changed ConfigMap is picked up without a restart.
type config struct {
	opts backend.KServeOptions

	mu        sync.Mutex
	cached    *settings
	fetchedAt time.Time
	now       func() time.Time
}

func newConfig(opts backend.KServeOptions) *config {
	return &config{opts: opts, now: time.Now}
}

// settings returns the effective configuration.
func (c *config) settings(ctx context.Context) settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.opts.DiscoveryTTL {
		return *c.cached
	}
	s := c.resolve(ctx)
	c.cached = &s
	c.fetchedAt = c.now()
	return s
}

// last returns the most recently resolved settings without refreshing
// (callers without a context); zero settings with defaults when none yet.
func (c *config) last() settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil {
		return *c.cached
	}
	s := settings{Namespace: DefaultNamespace}
	setIf(&s.Namespace, c.opts.Namespace)
	return s
}

func (c *config) resolve(ctx context.Context) settings {
	o := c.opts
	s := settings{
		Namespace:       DefaultNamespace,
		Runtime:         DefaultRuntime,
		GPUResourceName: DefaultGPUResourceName,
		CacheEnabled:    true,
		CacheClaim:      DefaultCacheClaim,
		CacheMountPath:  DefaultCacheMountPath,
		PresetNamespace: o.DiscoveryNamespace,
		PresetSelector:  DefaultPresetSelector,
	}
	if doc, err := c.discover(ctx); err != nil {
		s.DiscoveryError = err.Error()
	} else if doc != nil {
		s.DiscoveryFound = true
		sp := doc.Spec
		setIf(&s.Namespace, sp.Namespace)
		setIf(&s.Runtime, sp.Runtime)
		setIf(&s.GPUResourceName, sp.GPUResourceName)
		s.RuntimeClassName = sp.RuntimeClassName
		s.NodeSelector = sp.NodeSelector
		s.DeploymentStrategyType = sp.DeploymentStrategyType
		s.TimeoutSeconds = sp.TimeoutSeconds
		s.CacheEnabled = sp.Cache.Enabled
		setIf(&s.CacheClaim, sp.Cache.ClaimName)
		setIf(&s.CacheMountPath, sp.Cache.MountPath)
		s.CacheRedirectPolicy = sp.Cache.RedirectPolicy
		setIf(&s.PresetNamespace, sp.Presets.Namespace)
		setIf(&s.PresetSelector, sp.Presets.LabelSelector)
	}
	// Explicit options win over discovery.
	setIf(&s.Namespace, o.Namespace)
	setIf(&s.Runtime, o.Runtime)
	setIf(&s.GPUResourceName, o.GPUResourceName)
	if o.CacheClaim != "" {
		s.CacheClaim = o.CacheClaim
		s.CacheEnabled = true
	}
	setIf(&s.CacheMountPath, o.CacheMountPath)
	setIf(&s.PresetNamespace, o.PresetNamespace)
	setIf(&s.PresetSelector, o.PresetSelector)
	if s.PresetNamespace == "" {
		s.PresetNamespace = s.Namespace
	}
	return s
}

// discover reads the ModelServingConfig ConfigMap; nil when it does not exist.
func (c *config) discover(ctx context.Context) (*discoveryDoc, error) {
	o := c.opts
	if o.DiscoveryConfigMap == "" || o.DiscoveryNamespace == "" || o.Clientset == nil {
		return nil, nil
	}
	// The caller's clients when the request carries a caller token
	// (downstream OAuth: the ServiceAccount cannot read the ConfigMap).
	cs := o.Clientset
	if o.ClientsFor != nil {
		if c, _ := o.ClientsFor(ctx); c != nil {
			cs = c
		}
	}
	cm, err := cs.CoreV1().ConfigMaps(o.DiscoveryNamespace).Get(ctx, o.DiscoveryConfigMap, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read discovery ConfigMap %s/%s: %w", o.DiscoveryNamespace, o.DiscoveryConfigMap, err)
	}
	raw, ok := cm.Data[discoveryConfigKey]
	if !ok {
		return nil, fmt.Errorf("discovery ConfigMap %s/%s has no %s key", o.DiscoveryNamespace, o.DiscoveryConfigMap, discoveryConfigKey)
	}
	var doc discoveryDoc
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("parse discovery ConfigMap %s/%s: %w", o.DiscoveryNamespace, o.DiscoveryConfigMap, err)
	}
	if doc.Kind != "" && doc.Kind != discoveryKind {
		return nil, fmt.Errorf("discovery ConfigMap %s/%s: kind %q, want %s", o.DiscoveryNamespace, o.DiscoveryConfigMap, doc.Kind, discoveryKind)
	}
	return &doc, nil
}

func setIf(dst *string, v string) {
	if v = strings.TrimSpace(v); v != "" {
		*dst = v
	}
}

// defaultIf fills dst with def when it is empty.
func defaultIf(dst *string, def string) {
	if strings.TrimSpace(*dst) == "" {
		*dst = def
	}
}

// applyDefaults fills zero options.
func applyDefaults(o *backend.KServeOptions) {
	defaultIf(&o.DiscoveryConfigMap, DefaultDiscoveryConfigMap)
	defaultIf(&o.CacheIndexConfigMap, DefaultCacheIndexConfigMap)
	defaultIf(&o.HFEndpoint, DefaultHFEndpoint)
	defaultIf(&o.HFTokenSecretKey, DefaultHFTokenSecretKey)
	defaultIf(&o.DownloadImage, DefaultDownloadImage)
	defaultIf(&o.InitImage, DefaultInitImage)
	defaultIf(&o.BudgetSource, DefaultBudgetSource)
	defaultIf(&o.InventoryMode, InventoryModePod)
	defaultIf(&o.InventoryAgentSelector, DefaultInventoryAgentSelector)
	if o.InventoryAgentPort <= 0 {
		o.InventoryAgentPort = DefaultInventoryAgentPort
	}
	if o.DefaultOverheadGiB <= 0 {
		o.DefaultOverheadGiB = DefaultOverheadGiB
	}
	if o.DiscoveryTTL <= 0 {
		o.DiscoveryTTL = DefaultDiscoveryTTL
	}
	if o.InventoryTTL <= 0 {
		o.InventoryTTL = DefaultInventoryTTL
	}
	if o.InventoryTimeout <= 0 {
		o.InventoryTimeout = DefaultInventoryTimeout
	}
	if o.JobTTL <= 0 {
		o.JobTTL = DefaultJobTTL
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = DefaultReadyTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	o.HFEndpoint = strings.TrimRight(o.HFEndpoint, "/")
}
