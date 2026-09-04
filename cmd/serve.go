package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/backend/kserve"
	"github.com/giantswarm/model-manager/internal/backend/lemonade"
	"github.com/giantswarm/model-manager/internal/backend/ollama"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/kube"
	"github.com/giantswarm/model-manager/internal/server"
	"github.com/giantswarm/model-manager/internal/service"
	"github.com/giantswarm/model-manager/internal/wiring"
)

type serveOptions struct {
	listen string

	backendName           string
	backendNameSet        bool
	backends              string
	ollamaEndpoint        string
	ollamaAgentHost       string
	ollamaMemoryBudgetGiB string

	lemonadeEndpoint  string
	lemonadeAgentHost string

	kserve kserveFlags

	kubeconfig        string
	kubeContext       string
	inCluster         bool
	wiringDisabled    bool
	kagentNamespace   string
	kagentAPIVersion  string
	modelConfigPrefix string
	autoWire          bool
	defaultKeepAlive  string
	reconcileInterval time.Duration

	mcpEnabled bool
	mcpPath    string

	oauthEnabled                  bool
	oauthBaseURL                  string
	oauthProvider                 string
	dexIssuerURL                  string
	dexClientID                   string
	dexClientSecret               string
	dexCAFile                     string
	dexAllowPrivateIP             bool
	googleClientID                string
	googleClientSecret            string
	oauthTrustedAudiences         string
	ssoAllowPrivateIPs            bool
	allowPublicClientRegistration bool
	downstreamOAuth               bool

	jobRetention time.Duration
}

// kserveFlags are the kserve driver's settings. Empty serving-layer values are
// taken from the platform's discovery ConfigMap (kind ModelServingConfig).
type kserveFlags struct {
	discoveryNamespace  string
	discoveryConfigMap  string
	namespace           string
	runtime             string
	gpuResourceName     string
	cacheClaim          string
	cacheMountPath      string
	cacheNodes          string
	cacheIndexConfigMap string
	presetNamespace     string
	presetSelector      string
	hfEndpoint          string
	hfTokenSecret       string
	hfTokenSecretKey    string
	downloadImage       string
	downloadIgnore      string
	initImage           string
	jobTTL              time.Duration
	inventoryTTL        time.Duration
	inventoryTimeout    time.Duration
	inventoryMode       string
	inventorySelector   string
	inventoryAgentPort  int
	budgetSource        string
	defaultOverheadGiB  float64
	readyTimeout        time.Duration
	pollInterval        time.Duration
}

func newServeCmd() *cobra.Command {
	o := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the REST + MCP server",
		Long: `Run the model-manager server. Every flag can also be set through the
environment variable named next to it; flags win over the environment.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.backendNameSet = cmd.Flags().Changed("backend") || os.Getenv("MODEL_MANAGER_BACKEND") != ""
			return runServe(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("MODEL_MANAGER_LISTEN", ":8080"), "Listen address (MODEL_MANAGER_LISTEN)")
	f.StringVar(&o.backendName, "backend", envOr("MODEL_MANAGER_BACKEND", string(backend.NameOllama)), "Serving backend: ollama|kserve|lemonade — the one-backend form of --backends (MODEL_MANAGER_BACKEND)")
	f.StringVar(&o.backends, "backends", envOr("MODEL_MANAGER_BACKENDS", ""), "Comma-separated serving backends to run at once (ollama,lemonade,kserve), each at most once, in the operator's order: the first is the default backend, the one GET /api/v1/backend describes and an unqualified pull goes to. Empty runs --backend alone; when both are set the single value must be listed (MODEL_MANAGER_BACKENDS)")
	f.StringVar(&o.ollamaEndpoint, "ollama-endpoint", envOr("OLLAMA_ENDPOINT", "http://127.0.0.1:11434"), "Ollama API base URL as reached by model-manager (OLLAMA_ENDPOINT)")
	f.StringVar(&o.ollamaAgentHost, "ollama-agent-host", envOr("OLLAMA_AGENT_HOST", ""), "Ollama host written into kagent ModelConfigs, as reached by agent pods; defaults to --ollama-endpoint (OLLAMA_AGENT_HOST)")
	f.StringVar(&o.ollamaMemoryBudgetGiB, "ollama-memory-budget-gib", envOr("MODEL_MANAGER_OLLAMA_MEMORY_BUDGET_GIB", ""), "Memory budget of the proxied host in GiB (decimals allowed), reported on /api/v1/nodes as budgetSource=override instead of MemTotal of the pod's /proc/meminfo — for Docker Desktop, another VM-backed runtime or an Ollama on another machine; empty or 0: the pod's view (MODEL_MANAGER_OLLAMA_MEMORY_BUDGET_GIB)")
	f.StringVar(&o.lemonadeEndpoint, "lemonade-endpoint", envOr("LEMONADE_ENDPOINT", lemonade.DefaultEndpoint), "Lemonade Server base URL as reached by model-manager; its API is under /api/v1 (LEMONADE_ENDPOINT)")
	f.StringVar(&o.lemonadeAgentHost, "lemonade-agent-host", envOr("LEMONADE_AGENT_HOST", ""), "Lemonade Server base URL as reached by agent pods, written into kagent ModelConfigs as the OpenAI-compatible baseUrl with /api/v1 appended; defaults to --lemonade-endpoint (LEMONADE_AGENT_HOST)")

	k := &o.kserve
	f.StringVar(&k.discoveryNamespace, "kserve-discovery-namespace", envOr("KSERVE_DISCOVERY_NAMESPACE", envOr("POD_NAMESPACE", "")), "Namespace of the model-serving discovery ConfigMap; defaults to the pod's namespace (KSERVE_DISCOVERY_NAMESPACE, POD_NAMESPACE)")
	f.StringVar(&k.discoveryConfigMap, "kserve-discovery-configmap", envOr("KSERVE_DISCOVERY_CONFIGMAP", kserve.DefaultDiscoveryConfigMap), "Name of the discovery ConfigMap (kind ModelServingConfig, key config.yaml) (KSERVE_DISCOVERY_CONFIGMAP)")
	f.StringVar(&k.namespace, "kserve-namespace", envOr("KSERVE_NAMESPACE", ""), "Serving namespace for InferenceServices, download Jobs and the cache claim; empty: discovery, else "+kserve.DefaultNamespace+" (KSERVE_NAMESPACE)")
	f.StringVar(&k.runtime, "kserve-runtime", envOr("KSERVE_RUNTIME", ""), "ClusterServingRuntime for presets without one; empty: discovery, else "+kserve.DefaultRuntime+" (KSERVE_RUNTIME)")
	f.StringVar(&k.gpuResourceName, "kserve-gpu-resource", envOr("KSERVE_GPU_RESOURCE", ""), "Accelerator resource name; empty: discovery, else "+kserve.DefaultGPUResourceName+" (KSERVE_GPU_RESOURCE)")
	f.StringVar(&k.cacheClaim, "kserve-cache-claim", envOr("KSERVE_CACHE_CLAIM", ""), "PersistentVolumeClaim of the Hugging Face cache in the serving namespace; empty: discovery, else "+kserve.DefaultCacheClaim+" (KSERVE_CACHE_CLAIM)")
	f.StringVar(&k.cacheMountPath, "kserve-cache-mount-path", envOr("KSERVE_CACHE_MOUNT_PATH", ""), "Where predictors mount the cache; empty: discovery, else "+kserve.DefaultCacheMountPath+" (KSERVE_CACHE_MOUNT_PATH)")
	f.StringVar(&k.cacheNodes, "kserve-cache-nodes", envOr("KSERVE_CACHE_NODES", ""), "Comma-separated nodes holding the cache; empty derives them from the bound PersistentVolume's node affinity (KSERVE_CACHE_NODES)")
	f.StringVar(&k.cacheIndexConfigMap, "kserve-cache-index-configmap", envOr("KSERVE_CACHE_INDEX_CONFIGMAP", kserve.DefaultCacheIndexConfigMap), "ConfigMap in the serving namespace that remembers which Hugging Face repository filled which cache directory: recorded while an InferenceService serves from it, kept after it is gone (KSERVE_CACHE_INDEX_CONFIGMAP)")
	f.StringVar(&k.presetNamespace, "kserve-preset-namespace", envOr("KSERVE_PRESET_NAMESPACE", ""), "Namespace of the serving-preset ConfigMaps; empty: discovery, else the discovery namespace (KSERVE_PRESET_NAMESPACE)")
	f.StringVar(&k.presetSelector, "kserve-preset-selector", envOr("KSERVE_PRESET_SELECTOR", ""), "Label selector of the serving-preset ConfigMaps; empty: discovery, else "+kserve.DefaultPresetSelector+" (KSERVE_PRESET_SELECTOR)")
	f.StringVar(&k.hfEndpoint, "kserve-hf-endpoint", envOr("KSERVE_HF_ENDPOINT", kserve.DefaultHFEndpoint), "Hugging Face Hub base URL (KSERVE_HF_ENDPOINT)")
	f.StringVar(&k.hfTokenSecret, "kserve-hf-token-secret", envOr("KSERVE_HF_TOKEN_SECRET", ""), "Secret in the serving namespace holding a Hugging Face token for gated repositories (KSERVE_HF_TOKEN_SECRET)")
	f.StringVar(&k.hfTokenSecretKey, "kserve-hf-token-secret-key", envOr("KSERVE_HF_TOKEN_SECRET_KEY", kserve.DefaultHFTokenSecretKey), "Key of the token in that Secret (KSERVE_HF_TOKEN_SECRET_KEY)")
	f.StringVar(&k.downloadImage, "kserve-download-image", envOr("KSERVE_DOWNLOAD_IMAGE", kserve.DefaultDownloadImage), "Image of the pre-warm download Job (the KServe storage-initializer) (KSERVE_DOWNLOAD_IMAGE)")
	f.StringVar(&k.downloadIgnore, "kserve-download-ignore-patterns", envOr("KSERVE_DOWNLOAD_IGNORE_PATTERNS", ""), "Comma-separated file patterns downloads skip (STORAGE_IGNORE_PATTERNS); empty downloads the whole repository like an InferenceService does (KSERVE_DOWNLOAD_IGNORE_PATTERNS)")
	f.StringVar(&k.initImage, "kserve-init-image", envOr("KSERVE_INIT_IMAGE", kserve.DefaultInitImage), "Image that prepares cache directories and scans the cache (KSERVE_INIT_IMAGE)")
	f.DurationVar(&k.jobTTL, "kserve-job-ttl", envDuration("KSERVE_JOB_TTL", kserve.DefaultJobTTL), "ttlSecondsAfterFinished of download Jobs (KSERVE_JOB_TTL)")
	f.DurationVar(&k.inventoryTTL, "kserve-inventory-ttl", envDuration("KSERVE_INVENTORY_TTL", kserve.DefaultInventoryTTL), "How long a cache scan is reused (KSERVE_INVENTORY_TTL)")
	f.DurationVar(&k.inventoryTimeout, "kserve-inventory-timeout", envDuration("KSERVE_INVENTORY_TIMEOUT", kserve.DefaultInventoryTimeout), "Time budget of one cache scan (scan pod or cache-agent request) (KSERVE_INVENTORY_TIMEOUT)")
	f.StringVar(&k.inventoryMode, "kserve-inventory-mode", envOr("KSERVE_INVENTORY_MODE", kserve.InventoryModePod), "How a node's cache is read: pod (a short-lived scan pod per node) or daemonset (the cache-agent DaemonSet pod on the node, `model-manager cache-agent`) (KSERVE_INVENTORY_MODE)")
	f.StringVar(&k.inventorySelector, "kserve-inventory-agent-selector", envOr("KSERVE_INVENTORY_AGENT_SELECTOR", kserve.DefaultInventoryAgentSelector), "Label selector of the cache-agent pods in the serving namespace (daemonset mode) (KSERVE_INVENTORY_AGENT_SELECTOR)")
	f.IntVar(&k.inventoryAgentPort, "kserve-inventory-agent-port", envInt("KSERVE_INVENTORY_AGENT_PORT", kserve.DefaultInventoryAgentPort), "Port of the cache-agent pods (daemonset mode) (KSERVE_INVENTORY_AGENT_PORT)")
	f.StringVar(&k.budgetSource, "kserve-budget-source", envOr("KSERVE_BUDGET_SOURCE", kserve.DefaultBudgetSource), "Node memory budget: auto (GPU labels when present, else allocatable memory), gpu-labels, allocatable; the node annotation "+kserve.BudgetAnnotation+" (GiB) overrides it per node (KSERVE_BUDGET_SOURCE)")
	f.Float64Var(&k.defaultOverheadGiB, "kserve-default-overhead-gib", envFloat("KSERVE_DEFAULT_OVERHEAD_GIB", kserve.DefaultOverheadGiB), "Serving overhead added to the weights when the preset has none (KSERVE_DEFAULT_OVERHEAD_GIB)")
	f.DurationVar(&k.readyTimeout, "kserve-ready-timeout", envDuration("KSERVE_READY_TIMEOUT", kserve.DefaultReadyTimeout), "How long a load job waits for an InferenceService to become ready (KSERVE_READY_TIMEOUT)")
	f.DurationVar(&k.pollInterval, "kserve-poll-interval", envDuration("KSERVE_POLL_INTERVAL", kserve.DefaultPollInterval), "Poll period for Job progress and readiness (KSERVE_POLL_INTERVAL)")

	f.StringVar(&o.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "Kubeconfig path for agent wiring; empty uses the default loading rules or in-cluster auth (KUBECONFIG)")
	f.StringVar(&o.kubeContext, "kube-context", envOr("KUBE_CONTEXT", ""), "Kubeconfig context override (KUBE_CONTEXT)")
	f.BoolVar(&o.inCluster, "in-cluster", envBool("KUBERNETES_IN_CLUSTER", false), "Force in-cluster Kubernetes auth (KUBERNETES_IN_CLUSTER)")
	f.BoolVar(&o.wiringDisabled, "disable-wiring", envBool("MODEL_MANAGER_DISABLE_WIRING", false), "Do not touch kagent ModelConfigs at all; the wire capability reports false (MODEL_MANAGER_DISABLE_WIRING)")
	f.StringVar(&o.kagentNamespace, "kagent-namespace", envOr("KAGENT_NAMESPACE", "kagent"), "Namespace where ModelConfigs are created (KAGENT_NAMESPACE)")
	f.StringVar(&o.kagentAPIVersion, "kagent-api-version", envOr("KAGENT_API_VERSION", "auto"), "kagent.dev API version for ModelConfigs; auto discovers the server's preferred version (KAGENT_API_VERSION)")
	f.StringVar(&o.modelConfigPrefix, "modelconfig-prefix", envOr("MODELCONFIG_PREFIX", ""), "Prefix for generated ModelConfig names (MODELCONFIG_PREFIX)")
	f.BoolVar(&o.autoWire, "auto-wire", envBool("MODEL_MANAGER_AUTO_WIRE", true), "Create a ModelConfig when a pull completes or a model is loaded (kserve: when the served model is ready) (MODEL_MANAGER_AUTO_WIRE)")
	f.StringVar(&o.defaultKeepAlive, "default-keep-alive", envOr("MODEL_MANAGER_DEFAULT_KEEP_ALIVE", ollama.DefaultKeepAlive), "Default keep-alive for load requests (ollama; on lemonade only -1 has a meaning: it pins the model) (MODEL_MANAGER_DEFAULT_KEEP_ALIVE)")
	f.DurationVar(&o.reconcileInterval, "reconcile-interval", envDuration("MODEL_MANAGER_RECONCILE_INTERVAL", 30*time.Second), "How often served models are checked for a missing ModelConfig on backends that wire on readiness; 0 disables (MODEL_MANAGER_RECONCILE_INTERVAL)")
	f.BoolVar(&o.mcpEnabled, "mcp-enabled", envBool("MODEL_MANAGER_MCP_ENABLED", true), "Serve the MCP streamable-HTTP endpoint (MODEL_MANAGER_MCP_ENABLED)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("MODEL_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (MODEL_MANAGER_MCP_PATH)")
	f.BoolVar(&o.oauthEnabled, "enable-oauth", envBool("MODEL_MANAGER_OAUTH_ENABLED", false), "Require an OAuth 2.1 bearer token on the MCP endpoint and the REST API, validated against the platform IdP (mcp-oauth); the caller's identity travels with every request (MODEL_MANAGER_OAUTH_ENABLED)")
	f.StringVar(&o.oauthBaseURL, "oauth-base-url", envOr("MODEL_MANAGER_OAUTH_BASE_URL", ""), "Public base URL of this server: the issuer of its OAuth metadata, https or loopback http (MODEL_MANAGER_OAUTH_BASE_URL)")
	f.StringVar(&o.oauthProvider, "oauth-provider", envOr("MODEL_MANAGER_OAUTH_PROVIDER", server.ProviderDex), "Identity provider: dex or google (MODEL_MANAGER_OAUTH_PROVIDER)")
	f.StringVar(&o.dexIssuerURL, "dex-issuer-url", envOr("DEX_ISSUER_URL", ""), "Dex issuer URL (DEX_ISSUER_URL)")
	f.StringVar(&o.dexClientID, "dex-client-id", envOr("DEX_CLIENT_ID", ""), "Dex client ID (DEX_CLIENT_ID)")
	f.StringVar(&o.dexClientSecret, "dex-client-secret", envOr("DEX_CLIENT_SECRET", ""), "Dex client secret (DEX_CLIENT_SECRET)")
	f.StringVar(&o.dexCAFile, "dex-ca-file", envOr("DEX_CA_FILE", ""), "PEM CA bundle of a Dex with a private certificate; verifies discovery, token and JWKS calls (DEX_CA_FILE)")
	f.BoolVar(&o.dexAllowPrivateIP, "allow-private-oauth-urls", envBool("MODEL_MANAGER_OAUTH_ALLOW_PRIVATE_URLS", false), "Let the Dex issuer resolve to a private or loopback address, an in-cluster Dex (MODEL_MANAGER_OAUTH_ALLOW_PRIVATE_URLS)")
	f.StringVar(&o.googleClientID, "google-client-id", envOr("GOOGLE_CLIENT_ID", ""), "Google OAuth client ID (GOOGLE_CLIENT_ID)")
	f.StringVar(&o.googleClientSecret, "google-client-secret", envOr("GOOGLE_CLIENT_SECRET", ""), "Google OAuth client secret (GOOGLE_CLIENT_SECRET)")
	f.StringVar(&o.oauthTrustedAudiences, "oauth-trusted-audiences", envOr("OAUTH_TRUSTED_AUDIENCES", ""), "Comma-separated OAuth client IDs whose IdP id_tokens are accepted as bearer tokens — the platform client muster forwards tokens for and the portal logs in with (OAUTH_TRUSTED_AUDIENCES)")
	f.BoolVar(&o.ssoAllowPrivateIPs, "sso-allow-private-ips", envBool("SSO_ALLOW_PRIVATE_IPS", false), "Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (SSO_ALLOW_PRIVATE_IPS)")
	f.BoolVar(&o.allowPublicClientRegistration, "allow-public-client-registration", envBool("MODEL_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION", false), "Accept unauthenticated dynamic client registration; labs only (MODEL_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION)")
	f.BoolVar(&o.downstreamOAuth, "downstream-oauth", envBool("MODEL_MANAGER_DOWNSTREAM_OAUTH", false), "Call the Kubernetes API as the caller, with the caller's IdP token, for everything a request does — the ServiceAccount holds no permissions (the chart renders none) and work without a caller (download adoption, the wiring reconciler) is off. Needs --enable-oauth and an apiserver that trusts the IdP (MODEL_MANAGER_DOWNSTREAM_OAUTH)")
	f.DurationVar(&o.jobRetention, "job-retention", envDuration("MODEL_MANAGER_JOB_RETENTION", 24*time.Hour), "How long finished jobs stay listed (MODEL_MANAGER_JOB_RETENTION)")
	return cmd
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	if o.downstreamOAuth && !o.oauthEnabled {
		return fmt.Errorf("--downstream-oauth needs --enable-oauth: without OAuth there is no caller token to present to the Kubernetes API")
	}

	names, err := backendNames(o.backendName, o.backendNameSet, o.backends)
	if err != nil {
		return err
	}
	hasKServe := slices.Contains(names, backend.NameKServe)

	// Kubernetes access: required by the kserve driver, optional (wiring only)
	// for ollama and lemonade.
	var clients *kube.Clients
	if !o.wiringDisabled || hasKServe {
		c, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster, Logger: log})
		if err != nil {
			if hasKServe {
				return fmt.Errorf("the kserve backend needs Kubernetes access: %w", err)
			}
			log.Warn("agent wiring disabled: no Kubernetes access", "error", err)
		} else {
			clients = c
		}
	}

	backend.Register(backend.NameOllama, ollama.Factory)
	backend.Register(backend.NameKServe, kserve.Factory)
	backend.Register(backend.NameLemonade, lemonade.Factory)
	opts := backend.Options{
		Ollama:   backend.OllamaOptions{Endpoint: o.ollamaEndpoint, AgentHost: o.ollamaAgentHost, MemoryBudgetGiB: o.ollamaMemoryBudgetGiB},
		KServe:   o.kserve.options(),
		Lemonade: backend.LemonadeOptions{Endpoint: o.lemonadeEndpoint, AgentHost: o.lemonadeAgentHost},
	}
	if clients != nil {
		opts.KServe.Dynamic = clients.Dynamic
		opts.KServe.Clientset = clients.Clientset
		// Per-call clients: the caller's own when the request carries the
		// caller's token (downstream OAuth), the ServiceAccount's otherwise.
		opts.KServe.ClientsFor = func(ctx context.Context) (kubernetes.Interface, dynamic.Interface) {
			c := clients.For(ctx)
			return c.Clientset, c.Dynamic
		}
	}
	backends := make([]backend.Backend, 0, len(names))
	for _, name := range names {
		b, err := backend.New(name, opts)
		if err != nil {
			return err
		}
		log.Info("backend ready", "backend", b.Name(), "capabilities", b.Capabilities(), "default", len(backends) == 0)
		backends = append(backends, b)
	}

	var (
		wirer      wiring.Wirer
		wiringInfo *service.WiringInfo
	)
	switch {
	case o.wiringDisabled:
		log.Info("agent wiring disabled by flag")
	case clients == nil:
		// Already warned above.
	default:
		apiVersion := o.kagentAPIVersion
		if apiVersion == "" || apiVersion == "auto" {
			apiVersion, err = wiring.DiscoverAPIVersion(clients.Discovery)
			if err != nil {
				log.Warn("kagent API discovery failed, using default", "default", wiring.DefaultAPIVersion, "error", err)
				apiVersion = wiring.DefaultAPIVersion
			}
		}
		k := wiring.NewKagent(clients.Dynamic, o.kagentNamespace, apiVersion, o.modelConfigPrefix).
			WithClientFor(func(ctx context.Context) dynamic.Interface { return clients.For(ctx).Dynamic })
		wirer = k
		wiringInfo = &service.WiringInfo{Namespace: k.Namespace(), APIVersion: k.APIVersion()}
		log.Info("agent wiring enabled", "namespace", k.Namespace(), "apiVersion", k.APIVersion(), "autoWire", o.autoWire)
	}

	jm := jobs.NewManager(jobs.WithRetention(o.jobRetention))
	svc := service.New(backends, jm, wirer, wiringInfo, service.Config{AutoWire: o.autoWire, DefaultKeepAlive: o.defaultKeepAlive, ReconcileInterval: o.reconcileInterval, CallerOnly: o.downstreamOAuth}, log)

	cfg := server.Config{Addr: o.listen, MCPEnabled: o.mcpEnabled, MCPPath: o.mcpPath}
	if o.oauthEnabled {
		cfg.OAuth = &server.OAuthConfig{
			BaseURL:                       o.oauthBaseURL,
			Provider:                      o.oauthProvider,
			DexIssuerURL:                  o.dexIssuerURL,
			DexClientID:                   o.dexClientID,
			DexClientSecret:               o.dexClientSecret,
			DexCAFile:                     o.dexCAFile,
			DexAllowPrivateIP:             o.dexAllowPrivateIP,
			GoogleClientID:                o.googleClientID,
			GoogleClientSecret:            o.googleClientSecret,
			TrustedAudiences:              splitList(o.oauthTrustedAudiences),
			SSOAllowPrivateIPs:            o.ssoAllowPrivateIPs,
			AllowPublicClientRegistration: o.allowPublicClientRegistration,
			DownstreamOAuth:               o.downstreamOAuth,
		}
	}
	srv, err := server.New(cfg, svc, api.NewMCPServer(svc, version), log)
	if err != nil {
		return err
	}
	log.Info("model-manager starting", "version", version, "listen", o.listen, "rest", api.Prefix, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled, "oauth", o.oauthEnabled, "downstreamOAuth", o.downstreamOAuth)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.Run(ctx)
	if err := srv.Run(ctx); err != nil {
		return err
	}
	jm.Wait()
	return nil
}

// backendNames is the ordered list of drivers to run: --backends when set,
// else --backend alone. Names must be distinct; an explicit --backend next to
// --backends must be one of them.
func backendNames(single string, singleSet bool, list string) ([]backend.Name, error) {
	raw := splitList(list)
	if len(raw) == 0 {
		raw = []string{strings.TrimSpace(single)}
	} else if singleSet && !slices.Contains(raw, strings.TrimSpace(single)) {
		return nil, fmt.Errorf("--backend=%s is not in --backends=%s: name it in the list or drop the flag", single, list)
	}
	names := make([]backend.Name, 0, len(raw))
	for _, r := range raw {
		n := backend.Name(r)
		if slices.Contains(names, n) {
			return nil, fmt.Errorf("--backends lists %s twice: one process runs each driver at most once", n)
		}
		names = append(names, n)
	}
	return names, nil
}

func (k kserveFlags) options() backend.KServeOptions {
	return backend.KServeOptions{
		DiscoveryNamespace:     k.discoveryNamespace,
		DiscoveryConfigMap:     k.discoveryConfigMap,
		Namespace:              k.namespace,
		Runtime:                k.runtime,
		GPUResourceName:        k.gpuResourceName,
		CacheClaim:             k.cacheClaim,
		CacheMountPath:         k.cacheMountPath,
		CacheNodes:             splitList(k.cacheNodes),
		CacheIndexConfigMap:    k.cacheIndexConfigMap,
		PresetNamespace:        k.presetNamespace,
		PresetSelector:         k.presetSelector,
		HFEndpoint:             k.hfEndpoint,
		HFTokenSecret:          k.hfTokenSecret,
		HFTokenSecretKey:       k.hfTokenSecretKey,
		DownloadImage:          k.downloadImage,
		DownloadIgnorePatterns: splitList(k.downloadIgnore),
		InitImage:              k.initImage,
		JobTTL:                 k.jobTTL,
		InventoryTTL:           k.inventoryTTL,
		InventoryTimeout:       k.inventoryTimeout,
		InventoryMode:          k.inventoryMode,
		InventoryAgentSelector: k.inventorySelector,
		InventoryAgentPort:     k.inventoryAgentPort,
		BudgetSource:           k.budgetSource,
		DefaultOverheadGiB:     k.defaultOverheadGiB,
		ReadyTimeout:           k.readyTimeout,
		PollInterval:           k.pollInterval,
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
