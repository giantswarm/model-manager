package kserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
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
	// DefaultHFTimeout bounds the hub lookups of one fit check or search: the
	// hub answers in well under a second when reachable, and a hub that does
	// not answer must leave the preset fallback time to answer within a
	// meta-tool deadline (muster: 10 s).
	DefaultHFTimeout = 4 * time.Second
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
	// DefaultDownloadStallTimeout covers the hub's metadata phase and a slow
	// shard comfortably; a download the network silently drops shows nothing
	// for far longer.
	DefaultDownloadStallTimeout = 10 * time.Minute
	DefaultReadyTimeout         = 2 * time.Hour
	DefaultPollInterval         = 5 * time.Second
	discoveryConfigKey          = "config.yaml"
	discoveryKind               = "ModelServingConfig"
	presetConfigKey             = "preset.yaml"
	presetKind                  = "ServingPreset"
	agentPlatformAPIVersion     = "agent-platform.giantswarm.io/v1alpha1"
	budgetSourceAuto            = "auto"
	budgetSourceGPULabels       = "gpu-labels"
	budgetSourceAllocatable     = "allocatable"
	// budgetSourceAnnotation is reported when the node's BudgetAnnotation
	// overrode the configured source.
	budgetSourceAnnotation       = "annotation"
	gib                    int64 = 1 << 30

	// ServingKindLLM composes presets into LLMInferenceServices
	// (serving.kserve.io/v1alpha2, the llm-d control plane); ServingKindClassic
	// into InferenceServices on the vLLM ClusterServingRuntime; ServingKindAuto
	// picks the first wherever its API is served.
	ServingKindAuto    = "auto"
	ServingKindLLM     = "LLMInferenceService"
	ServingKindClassic = "InferenceService"
)

// validServingKind reports whether k is one of the three ServingKind values.
func validServingKind(k string) bool {
	switch k {
	case ServingKindAuto, ServingKindLLM, ServingKindClassic:
		return true
	}
	return false
}

// discoveryDoc is the ModelServingConfig document the platform's connectivity
// chart publishes (agent-platform-connectivity, templates/model-serving/config.yaml).
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
		// GPUPool is the pool taint and label (the chart's
		// modelServing.gpuPool); see backend.GPUPool.
		GPUPool backend.GPUPool `json:"gpuPool"`
		Cache   struct {
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
		// Gateway is the models Gateway every LLMInferenceService route
		// attaches to: Endpoint its public origin (https://models.<domain>),
		// PathConvention the path a served model answers on
		// (/<namespace>/<model>/v1).
		Gateway struct {
			Enabled        bool   `json:"enabled"`
			Name           string `json:"name"`
			Namespace      string `json:"namespace"`
			Endpoint       string `json:"endpoint"`
			PathConvention string `json:"pathConvention"`
		} `json:"gateway"`
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
	// GPUPool is the pool scheduling every scan pod, download Job and
	// composed predictor gets (scheduling.go): discovery's spec.gpuPool,
	// the option's taint and selector replacing each when set.
	GPUPool backend.GPUPool
	// RouterScheduler composes the llm-d endpoint picker (router.scheduler)
	// beside the route on every LLMInferenceService whose preset does not
	// decide for itself (llmisvc.go): the option's; discovery has no say.
	RouterScheduler bool
	// GatewayEndpoint is the models Gateway's origin (https://models.<domain>,
	// no trailing slash) from discovery's spec.gateway when the Gateway is
	// enabled, empty otherwise. KServe routes every LLMInferenceService on it
	// at <origin>/<namespace>/<name>, so the address a served model gets is
	// known the moment the object is composed — before KServe publishes it in
	// status.addresses (giantswarm/model-manager#115).
	GatewayEndpoint     string
	CacheEnabled        bool
	CacheClaim          string
	CacheMountPath      string
	CacheRedirectPolicy bool
	PresetNamespace     string
	PresetSelector      string
	// ServingKind is the kind presets are composed into, ServingKindLLM or
	// ServingKindClassic (never auto); LLMServed whether the
	// LLMInferenceService API is served on the cluster, in which case the
	// serving namespace's LLMInferenceServices are listed too.
	ServingKind string
	LLMServed   bool
	// DiscoveryFound reports whether the discovery ConfigMap was read.
	DiscoveryFound bool
	DiscoveryError string
}

// config resolves settings from options plus the discovery ConfigMap, cached
// for DiscoveryTTL so a changed ConfigMap is picked up without a restart.
type config struct {
	opts backend.KServeOptions
	log  *slog.Logger

	mu        sync.Mutex
	cached    *settings
	fetchedAt time.Time
	now       func() time.Time
	// refreshErr is the failure the last refresh ended in ("" when it
	// succeeded); a change is logged once, not every attempt.
	refreshErr string
}

func newConfig(opts backend.KServeOptions, log *slog.Logger) *config {
	if log == nil {
		log = slog.Default()
	}
	return &config{opts: opts, log: log, now: time.Now}
}

// settings returns the effective configuration. The cache is shared by every
// caller, so a refresh only replaces it when it succeeds: a refresh that
// fails with the credentials at hand — a job whose caller token expired, a
// caller without the permission — keeps the last good settings and is tried
// again on the next call. Before this rule a load job polling with a dead
// token cached "LLMInferenceService API not served" for everyone, and the
// namespace's LLMInferenceServices vanished from every list and unload
// (giantswarm/model-manager#92).
func (c *config) settings(ctx context.Context) settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.opts.DiscoveryTTL {
		return *c.cached
	}
	s, err := c.resolve(ctx)
	c.noteRefresh(err)
	switch {
	case err == nil:
		c.cached = &s
		c.fetchedAt = c.now()
	case c.cached != nil:
		return *c.cached
	}
	return s
}

// noteRefresh logs a refresh failure when it starts and the recovery when it
// ends; the attempts in between stay quiet.
func (c *config) noteRefresh(err error) {
	switch {
	case err == nil && c.refreshErr != "":
		c.refreshErr = ""
		c.log.Info("serving settings refreshed again")
	case err != nil && err.Error() != c.refreshErr:
		c.refreshErr = err.Error()
		if c.cached != nil {
			c.log.Warn("refreshing the serving settings failed; keeping the last good ones", "error", err, "age", c.now().Sub(c.fetchedAt).Round(time.Second))
		} else {
			c.log.Warn("resolving the serving settings failed; using the defaults until a refresh succeeds", "error", err)
		}
	}
}

// last returns the most recently resolved settings without refreshing
// (callers without a context); zero settings with defaults when none yet.
func (c *config) last() settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil {
		return *c.cached
	}
	s := settings{Namespace: DefaultNamespace, ServingKind: ServingKindClassic}
	setIf(&s.Namespace, c.opts.Namespace)
	if c.opts.ServingKind == ServingKindLLM {
		s.ServingKind = ServingKindLLM
	}
	return s
}

// resolve reads the settings once. The error says the answer is incomplete —
// the discovery ConfigMap or the API discovery could not be read with any
// client at hand — and must not be cached; the settings returned beside it
// are the defaults plus whatever was read.
func (c *config) resolve(ctx context.Context) (settings, error) {
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
	var errs []error
	if doc, err := c.discover(ctx); err != nil {
		s.DiscoveryError = err.Error()
		errs = append(errs, err)
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
		s.GPUPool = sp.GPUPool
		s.CacheEnabled = sp.Cache.Enabled
		setIf(&s.CacheClaim, sp.Cache.ClaimName)
		setIf(&s.CacheMountPath, sp.Cache.MountPath)
		s.CacheRedirectPolicy = sp.Cache.RedirectPolicy
		setIf(&s.PresetNamespace, sp.Presets.Namespace)
		setIf(&s.PresetSelector, sp.Presets.LabelSelector)
		if sp.Gateway.Enabled {
			s.GatewayEndpoint = strings.TrimRight(strings.TrimSpace(sp.Gateway.Endpoint), "/")
		}
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
	if o.GPUPool.Taint != nil {
		s.GPUPool.Taint = o.GPUPool.Taint
	}
	if len(o.GPUPool.NodeSelector) > 0 {
		s.GPUPool.NodeSelector = o.GPUPool.NodeSelector
	}
	if len(o.GPUPool.Instances) > 0 {
		s.GPUPool.Instances = o.GPUPool.Instances
	}
	s.RouterScheduler = o.Router.Scheduler
	// The document's shapes were validated when it was read; discovery's
	// come from chart values nothing has checked. A list with a bad shape
	// is dropped rather than judged on: the answer falls back to unverified.
	if err := backend.ValidateInstances(s.GPUPool.Instances); err != nil {
		c.log.Warn("ignoring the GPU pool's instance shapes", "error", "gpuPool."+err.Error())
		s.GPUPool.Instances = nil
	}
	if s.PresetNamespace == "" {
		s.PresetNamespace = s.Namespace
	}
	served, err := c.apiServed(ctx, llmisvcGVR)
	if err != nil {
		errs = append(errs, err)
	}
	s.LLMServed = served
	s.ServingKind = o.ServingKind
	if s.ServingKind == ServingKindAuto || s.ServingKind == "" {
		s.ServingKind = ServingKindClassic
		if s.LLMServed {
			s.ServingKind = ServingKindLLM
		}
	}
	return s, errors.Join(errs...)
}

// clientsets returns the clientsets a resolve tries, in order: the caller's
// when the request carries a caller token (downstream OAuth: only the caller
// may read the ConfigMap; a remote target knows no other credential), then
// the configured one, which still answers when the caller's token has
// expired. Empty without either.
func (c *config) clientsets(ctx context.Context) []kubernetes.Interface {
	var out []kubernetes.Interface
	if c.opts.ClientsFor != nil {
		if caller, _ := c.opts.ClientsFor(ctx); caller != nil && caller != c.opts.Clientset {
			out = append(out, caller)
		}
	}
	if c.opts.Clientset != nil {
		out = append(out, c.opts.Clientset)
	}
	return out
}

// apiServed reports whether the API server serves gvr. The first client whose
// discovery request is answered decides — the group version listed, or not
// found. An error from every client means the answer is unknown, never that
// the kind is absent.
func (c *config) apiServed(ctx context.Context, gvr schema.GroupVersionResource) (bool, error) {
	var errs []error
	for _, cs := range c.clientsets(ctx) {
		list, err := cs.Discovery().ServerResourcesForGroupVersion(gvr.GroupVersion().String())
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range list.APIResources {
			if r.Name == gvr.Resource {
				return true, nil
			}
		}
		return false, nil
	}
	if len(errs) == 0 {
		return false, nil
	}
	return false, fmt.Errorf("discover %s: %w", gvr.GroupVersion(), errors.Join(errs...))
}

// discover reads the ModelServingConfig ConfigMap; nil when it does not exist.
// The first client that reads it, or finds it absent, decides; an error from
// every client is returned.
func (c *config) discover(ctx context.Context) (*discoveryDoc, error) {
	o := c.opts
	if o.DiscoveryConfigMap == "" || o.DiscoveryNamespace == "" {
		return nil, nil
	}
	var (
		cm   *corev1.ConfigMap
		errs []error
	)
	for _, cs := range c.clientsets(ctx) {
		got, err := cs.CoreV1().ConfigMaps(o.DiscoveryNamespace).Get(ctx, o.DiscoveryConfigMap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cm = got
		break
	}
	if cm == nil {
		if len(errs) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("read discovery ConfigMap %s/%s: %w", o.DiscoveryNamespace, o.DiscoveryConfigMap, errors.Join(errs...))
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
	defaultIf(&o.ServingKind, ServingKindAuto)
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
	if o.HFTimeout <= 0 {
		o.HFTimeout = DefaultHFTimeout
	}
	if o.JobTTL <= 0 {
		o.JobTTL = DefaultJobTTL
	}
	if o.DownloadStallTimeout <= 0 {
		o.DownloadStallTimeout = DefaultDownloadStallTimeout
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = DefaultReadyTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	o.HFEndpoint = strings.TrimRight(o.HFEndpoint, "/")
}
