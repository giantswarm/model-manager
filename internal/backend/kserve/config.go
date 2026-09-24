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
	"k8s.io/client-go/dynamic"
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
	// DefaultDownloadImage is the KServe storage-initializer, from the
	// platform's registry like every image the platform runs and at the version
	// the platform's KServe chart injects into a predictor: a pre-warm download
	// then produces exactly the files an LLMInferenceService's own download
	// would, so a later start finds them and skips the download.
	DefaultDownloadImage = "gsoci.azurecr.io/giantswarm/storage-initializer:v0.20.0"
	// DefaultInitImage creates cache directories and scans the cache.
	DefaultInitImage    = "gsoci.azurecr.io/giantswarm/alpine:3.22.1"
	DefaultBudgetSource = "auto"
	DefaultOverheadGiB  = 30
	DefaultDiscoveryTTL = time.Minute
	// DiscoveryAbsentTTL bounds how long settings resolved without the
	// configured discovery ConfigMap stand (config.fresh): the document
	// appears when the serving slice's connectivity child installs, and a
	// fit judged on settings that predate it knows no GPU pool
	// (giantswarm/model-manager#127). A resolve error is never cached.
	DiscoveryAbsentTTL = 5 * time.Second
	// ControlPlaneAbsentTTL bounds how long settings resolved without the
	// llm-d control plane stand (config.fresh) — the LLMInferenceService API
	// not served, or served by the CRDs alone without the well-known
	// LLMInferenceServiceConfig. The serving slice's CRD and runtime-configs
	// children land seconds after its discovery document, and a load judged
	// on settings cached in between was refused for the full DiscoveryTTL
	// with a hint to turn on components that were on
	// (giantswarm/model-manager#148).
	ControlPlaneAbsentTTL   = DiscoveryAbsentTTL
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
	// DefaultScaleUpTimeout is the GPU pool's scale-up budget: a node the
	// autoscaler launches registers in ≈ 3.5 min, and a capacity refusal is
	// retried every ≈ 3 min — ten minutes without a node say the pool cannot
	// launch one as it stands.
	DefaultScaleUpTimeout   = 10 * time.Minute
	DefaultPollInterval     = 5 * time.Second
	discoveryConfigKey      = "config.yaml"
	discoveryKind           = "ModelServingConfig"
	presetConfigKey         = "preset.yaml"
	presetKind              = "ServingPreset"
	agentPlatformAPIVersion = "agent-platform.giantswarm.io/v1alpha1"
	budgetSourceAuto        = "auto"
	budgetSourceGPULabels   = "gpu-labels"
	budgetSourceAllocatable = "allocatable"
	// budgetSourceAnnotation is reported when the node's BudgetAnnotation
	// overrode the configured source.
	budgetSourceAnnotation       = "annotation"
	gib                    int64 = 1 << 30

	// kindLLMInferenceService is the one kind the driver composes, lists and
	// deletes: KServe's llm-d control plane (serving.kserve.io/v1alpha2).
	kindLLMInferenceService = "LLMInferenceService"
	// wellKnownTemplateConfig is the LLMInferenceServiceConfig the controller
	// composes every LLMInferenceService's workload from — the template that
	// names the runtime image and the main container the preset's args, env
	// and resources go into. It ships with the llm-d control plane (the
	// platform's kserve-runtime-configs component), not with the CRDs, so
	// its presence says a controller is there to reconcile what the driver
	// creates (giantswarm/model-manager#129).
	wellKnownTemplateConfig = "kserve-config-llm-template"
)

// discoveryDoc is the ModelServingConfig document the platform's connectivity
// chart publishes (agent-platform-connectivity, templates/model-serving/config.yaml).
type discoveryDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Namespace        string            `json:"namespace"`
		GPUResourceName  string            `json:"gpuResourceName"`
		RuntimeClassName string            `json:"runtimeClassName"`
		NodeSelector     map[string]string `json:"nodeSelector"`
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
		// LLMEndpoint is the platform's LLM endpoint (the chart's
		// llmRouting): the route(s) a served model's AgentgatewayModel
		// attaches to (ParentRefs, Gateway API parent references) and the
		// listener's in-cluster URL (Endpoint).
		LLMEndpoint struct {
			Enabled    bool             `json:"enabled"`
			ParentRefs []map[string]any `json:"parentRefs"`
			Endpoint   string           `json:"endpoint"`
		} `json:"llmEndpoint"`
	} `json:"spec"`
}

// settings is the effective serving-layer configuration: discovery merged
// with explicit options (options win).
type settings struct {
	Namespace        string
	GPUResourceName  string
	RuntimeClassName string
	NodeSelector     map[string]string
	// GPUPool is the pool scheduling every scan pod, download Job and
	// composed workload gets (scheduling.go): discovery's spec.gpuPool,
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
	GatewayEndpoint string
	// LLMEndpoint puts every served model on the platform's LLM endpoint
	// (gatewaymodel.go): discovery's spec.llmEndpoint when enabled with a
	// route and a URL, nil otherwise.
	LLMEndpoint         *llmEndpoint
	CacheEnabled        bool
	CacheClaim          string
	CacheMountPath      string
	CacheRedirectPolicy bool
	PresetNamespace     string
	PresetSelector      string
	// LLMServed reports whether the LLMInferenceService API is served on the
	// cluster; ControlPlane is the namespace holding the well-known
	// LLMInferenceServiceConfig the llm-d controller composes from, empty
	// when the CRDs stand alone and nothing would reconcile a created object.
	// ServingError is why neither could be established ("" when they were):
	// a load is refused on it rather than judged on a guess.
	LLMServed    bool
	ControlPlane string
	ServingError string
	// DiscoveryFound reports whether the discovery ConfigMap was read.
	DiscoveryFound bool
	DiscoveryError string
}

// forPreset is the settings a preset is judged and composed with. A preset
// that requests no GPU (resources.gpus: 0, servingPreset.cpu) knows no GPU
// pool and no accelerator RuntimeClass: its predictor runs on the cluster's
// CPU capacity, so the pool's taint is not tolerated, the pool's label not
// selected and the RuntimeClass not set — every other setting stands. Every
// other preset gets the settings as they are.
func (s settings) forPreset(p *servingPreset) settings {
	if !p.cpu() {
		return s
	}
	s.GPUPool = backend.GPUPool{}
	s.RuntimeClassName = ""
	return s
}

// servingUnavailable is why a load cannot be served on this cluster as the
// settings stand — the LLMInferenceService API is not served, the llm-d
// controller is not installed, or neither could be read — and what to do
// about it; "" when a load can go ahead. The check runs before anything is
// composed or created, so a cluster carrying the CRDs without the controller
// gets a refusal instead of an object nothing reconciles and a load job that
// waits forever (giantswarm/model-manager#129).
func (s settings) servingUnavailable() string {
	gv := llmisvcGVR.GroupVersion().String()
	switch {
	case s.ServingError != "":
		return fmt.Sprintf("cannot tell whether the llm-d control plane serves this cluster: %s; the driver needs to read the %s API and list %s cluster-wide", s.ServingError, gv, llmisvcConfigGVR.Resource)
	case !s.LLMServed:
		return fmt.Sprintf("the %s API (%s) is not served on this cluster%s", kindLLMInferenceService, gv, s.controlPlaneHint("kserve-llmisvc-crd, kserve-llmisvc-resources and kserve-runtime-configs"))
	case s.ControlPlane == "":
		return fmt.Sprintf("the %s API is served but the llm-d controller is not installed: no LLMInferenceServiceConfig %s in any namespace, so nothing would reconcile a created object%s", kindLLMInferenceService, wellKnownTemplateConfig, s.controlPlaneHint("kserve-llmisvc-resources and kserve-runtime-configs"))
	}
	return ""
}

// controlPlaneAbsent reports whether s was resolved without the llm-d control
// plane — the LLMInferenceService API not served, or the well-known config
// absent — as a result, not an error: what the serving slice's CRD and
// runtime-configs children change as they land.
func (s settings) controlPlaneAbsent() bool {
	return s.ServingError == "" && (!s.LLMServed || s.ControlPlane == "")
}

// controlPlaneHint is what to do about the missing llm-d control plane,
// naming the platform components that install it. With the serving layer's
// discovery document published, the slice that publishes it is most likely
// still landing — its CRD and runtime-configs children follow the document by
// seconds — so the hint is to retry; without the document, the components are
// off (giantswarm/model-manager#148).
func (s settings) controlPlaneHint(components string) string {
	if s.DiscoveryFound {
		return fmt.Sprintf("; the serving layer's discovery document is published, so its slice is most likely still landing: retry in a moment, and turn on the platform's %s components if this persists", components)
	}
	return fmt.Sprintf("; install the llm-d control plane — the platform's %s components", components)
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
	if c.fresh() {
		return *c.cached
	}
	s, err := c.refresh(ctx)
	if err != nil && c.cached != nil {
		return *c.cached
	}
	return s
}

// fresh reports whether the cached settings still stand: for DiscoveryTTL —
// or for DiscoveryAbsentTTL when they were resolved without the configured
// discovery ConfigMap, and for ControlPlaneAbsentTTL when without the llm-d
// control plane. The serving slice publishes that document as it installs,
// and settings cached a moment before it appeared know no GPU pool: a fit
// judged on them for the full TTL refused a pool that scales from zero with
// "no accelerator node" (giantswarm/model-manager#127). The slice's CRDs and
// runtime configs follow the document by seconds, and a load judged on
// settings cached in between was refused as if the components were off
// (giantswarm/model-manager#148).
func (c *config) fresh() bool {
	if c.cached == nil {
		return false
	}
	ttl := c.opts.DiscoveryTTL
	if c.discoveryAbsent(*c.cached) {
		ttl = min(ttl, DiscoveryAbsentTTL)
	}
	if c.cached.controlPlaneAbsent() {
		ttl = min(ttl, ControlPlaneAbsentTTL)
	}
	return c.now().Sub(c.fetchedAt) < ttl
}

// settingsForLoad returns the settings a load is judged on: the cached ones
// while they stand and say a control plane would reconcile the object, else
// resolved once more first. A refusal is given on what the cluster says now,
// not on a verdict cached moments before the slice's runtime configs landed
// (giantswarm/model-manager#148); a resolve that fails keeps the last good
// settings, as settings does.
func (c *config) settingsForLoad(ctx context.Context) settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fresh() && !c.cached.controlPlaneAbsent() {
		return *c.cached
	}
	s, err := c.refresh(ctx)
	if err != nil && c.cached != nil {
		return *c.cached
	}
	return s
}

// refresh resolves the settings and caches them when the resolve succeeded.
// The caller holds c.mu.
func (c *config) refresh(ctx context.Context) (settings, error) {
	s, err := c.resolve(ctx)
	c.noteRefresh(err)
	if err == nil {
		c.cached = &s
		c.fetchedAt = c.now()
	}
	return s, err
}

// recheckDiscovery re-resolves the settings once, bypassing the cache, when
// they were resolved without the configured discovery ConfigMap, and reports
// whether the document is there now. A fit that found no node asks before it
// refuses: the document, and with it the GPU pool the fit is judged against,
// may have appeared since the settings were cached
// (giantswarm/model-manager#127).
func (c *config) recheckDiscovery(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.discoveryConfigured() || (c.cached != nil && c.cached.DiscoveryFound) {
		return false
	}
	s, err := c.refresh(ctx)
	return err == nil && s.DiscoveryFound
}

// discoveryConfigured reports whether a discovery ConfigMap is named at all.
func (c *config) discoveryConfigured() bool {
	return c.opts.DiscoveryConfigMap != "" && c.opts.DiscoveryNamespace != ""
}

// discoveryAbsent reports whether s was resolved with the configured
// discovery ConfigMap absent: the document is not published (yet).
func (c *config) discoveryAbsent(s settings) bool {
	return c.discoveryConfigured() && !s.DiscoveryFound && s.DiscoveryError == ""
}

// discoveryMissing is the clause a refusal carries when the settings it was
// judged on lack the configured discovery document — not published yet, or
// unreadable — and says to retry; empty when the document was read or none
// is configured.
func (c *config) discoveryMissing(s settings) string {
	switch {
	case !c.discoveryConfigured() || s.DiscoveryFound:
		return ""
	case s.DiscoveryError != "":
		return fmt.Sprintf("the serving layer's discovery document could not be read (%s); retry in a moment", s.DiscoveryError)
	}
	return fmt.Sprintf("the serving layer's discovery document %s/%s is not published yet — the slice is still installing; retry in a moment", c.opts.DiscoveryNamespace, c.opts.DiscoveryConfigMap)
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
	s := settings{Namespace: DefaultNamespace}
	setIf(&s.Namespace, c.opts.Namespace)
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
		setIf(&s.GPUResourceName, sp.GPUResourceName)
		s.RuntimeClassName = sp.RuntimeClassName
		s.NodeSelector = sp.NodeSelector
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
		if le := sp.LLMEndpoint; le.Enabled {
			ep, err := newLLMEndpoint(le.ParentRefs, le.Endpoint, o.DiscoveryNamespace)
			if err != nil {
				c.log.Warn("discovery's spec.llmEndpoint is unusable; served models stay off the LLM endpoint", "error", err)
			}
			s.LLMEndpoint = ep
		}
	}
	// Explicit options win over discovery.
	setIf(&s.Namespace, o.Namespace)
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
		s.ServingError = err.Error()
		errs = append(errs, err)
	}
	s.LLMServed = served
	if served {
		s.ControlPlane, err = c.controlPlane(ctx)
		if err != nil {
			s.ServingError = err.Error()
			errs = append(errs, err)
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

// dynamics returns the dynamic clients a resolve tries, in the order of
// clientsets: the caller's, then the configured one.
func (c *config) dynamics(ctx context.Context) []dynamic.Interface {
	var out []dynamic.Interface
	if c.opts.ClientsFor != nil {
		if _, caller := c.opts.ClientsFor(ctx); caller != nil && caller != c.opts.Dynamic {
			out = append(out, caller)
		}
	}
	if c.opts.Dynamic != nil {
		out = append(out, c.opts.Dynamic)
	}
	return out
}

// controlPlane looks for the llm-d controller by the well-known
// LLMInferenceServiceConfig it composes from (wellKnownTemplateConfig) and
// returns the namespace holding it, "" when no namespace does. The controller
// reads its configs from the object's namespace, then from its own, so the
// lookup is cluster-wide and takes the first client that answers; an error
// from every client means the answer is unknown, never that the controller
// is absent.
func (c *config) controlPlane(ctx context.Context) (string, error) {
	var errs []error
	for _, dyn := range c.dynamics(ctx) {
		list, err := dyn.Resource(llmisvcConfigGVR).List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + wellKnownTemplateConfig})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for i := range list.Items {
			if list.Items[i].GetName() == wellKnownTemplateConfig {
				return list.Items[i].GetNamespace(), nil
			}
		}
		return "", nil
	}
	if len(errs) == 0 {
		return "", nil
	}
	return "", fmt.Errorf("list %s: %w", llmisvcConfigGVR.Resource, errors.Join(errs...))
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
	if o.ScaleUpTimeout <= 0 {
		o.ScaleUpTimeout = DefaultScaleUpTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	o.HFEndpoint = strings.TrimRight(o.HFEndpoint, "/")
}
