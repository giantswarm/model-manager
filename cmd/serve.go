package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/backend/ollama"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/kube"
	"github.com/giantswarm/model-manager/internal/server"
	"github.com/giantswarm/model-manager/internal/service"
	"github.com/giantswarm/model-manager/internal/wiring"
)

type serveOptions struct {
	listen string

	backendName     string
	ollamaEndpoint  string
	ollamaAgentHost string

	kubeconfig        string
	kubeContext       string
	inCluster         bool
	wiringDisabled    bool
	kagentNamespace   string
	kagentAPIVersion  string
	modelConfigPrefix string
	autoWire          bool
	defaultKeepAlive  string

	mcpEnabled bool
	mcpPath    string

	oauthEnabled    bool
	oauthBaseURL    string
	dexIssuerURL    string
	dexClientID     string
	dexClientSecret string

	jobRetention time.Duration
}

func newServeCmd() *cobra.Command {
	o := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the REST + MCP server",
		Long: `Run the model-manager server. Every flag can also be set through the
environment variable named next to it; flags win over the environment.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("MODEL_MANAGER_LISTEN", ":8080"), "Listen address (MODEL_MANAGER_LISTEN)")
	f.StringVar(&o.backendName, "backend", envOr("MODEL_MANAGER_BACKEND", string(backend.NameOllama)), "Serving backend: ollama|kserve (MODEL_MANAGER_BACKEND)")
	f.StringVar(&o.ollamaEndpoint, "ollama-endpoint", envOr("OLLAMA_ENDPOINT", "http://127.0.0.1:11434"), "Ollama API base URL as reached by model-manager (OLLAMA_ENDPOINT)")
	f.StringVar(&o.ollamaAgentHost, "ollama-agent-host", envOr("OLLAMA_AGENT_HOST", ""), "Ollama host written into kagent ModelConfigs, as reached by agent pods; defaults to --ollama-endpoint (OLLAMA_AGENT_HOST)")
	f.StringVar(&o.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "Kubeconfig path for agent wiring; empty uses the default loading rules or in-cluster auth (KUBECONFIG)")
	f.StringVar(&o.kubeContext, "kube-context", envOr("KUBE_CONTEXT", ""), "Kubeconfig context override (KUBE_CONTEXT)")
	f.BoolVar(&o.inCluster, "in-cluster", envBool("KUBERNETES_IN_CLUSTER", false), "Force in-cluster Kubernetes auth (KUBERNETES_IN_CLUSTER)")
	f.BoolVar(&o.wiringDisabled, "disable-wiring", envBool("MODEL_MANAGER_DISABLE_WIRING", false), "Do not touch kagent ModelConfigs at all; the wire capability reports false (MODEL_MANAGER_DISABLE_WIRING)")
	f.StringVar(&o.kagentNamespace, "kagent-namespace", envOr("KAGENT_NAMESPACE", "kagent"), "Namespace where ModelConfigs are created (KAGENT_NAMESPACE)")
	f.StringVar(&o.kagentAPIVersion, "kagent-api-version", envOr("KAGENT_API_VERSION", "auto"), "kagent.dev API version for ModelConfigs; auto discovers the server's preferred version (KAGENT_API_VERSION)")
	f.StringVar(&o.modelConfigPrefix, "modelconfig-prefix", envOr("MODELCONFIG_PREFIX", ""), "Prefix for generated ModelConfig names (MODELCONFIG_PREFIX)")
	f.BoolVar(&o.autoWire, "auto-wire", envBool("MODEL_MANAGER_AUTO_WIRE", true), "Create a ModelConfig when a pull completes or a model is loaded (MODEL_MANAGER_AUTO_WIRE)")
	f.StringVar(&o.defaultKeepAlive, "default-keep-alive", envOr("MODEL_MANAGER_DEFAULT_KEEP_ALIVE", ollama.DefaultKeepAlive), "Default keep-alive for load requests (MODEL_MANAGER_DEFAULT_KEEP_ALIVE)")
	f.BoolVar(&o.mcpEnabled, "mcp-enabled", envBool("MODEL_MANAGER_MCP_ENABLED", true), "Serve the MCP streamable-HTTP endpoint (MODEL_MANAGER_MCP_ENABLED)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("MODEL_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (MODEL_MANAGER_MCP_PATH)")
	f.BoolVar(&o.oauthEnabled, "enable-oauth", envBool("MODEL_MANAGER_OAUTH_ENABLED", false), "Protect the MCP endpoint with an embedded OAuth 2.1 server backed by Dex (MODEL_MANAGER_OAUTH_ENABLED)")
	f.StringVar(&o.oauthBaseURL, "oauth-base-url", envOr("MODEL_MANAGER_OAUTH_BASE_URL", ""), "Public base URL of this server for OAuth (MODEL_MANAGER_OAUTH_BASE_URL)")
	f.StringVar(&o.dexIssuerURL, "dex-issuer-url", envOr("DEX_ISSUER_URL", ""), "Dex issuer URL (DEX_ISSUER_URL)")
	f.StringVar(&o.dexClientID, "dex-client-id", envOr("DEX_CLIENT_ID", ""), "Dex client ID (DEX_CLIENT_ID)")
	f.StringVar(&o.dexClientSecret, "dex-client-secret", envOr("DEX_CLIENT_SECRET", ""), "Dex client secret (DEX_CLIENT_SECRET)")
	f.DurationVar(&o.jobRetention, "job-retention", envDuration("MODEL_MANAGER_JOB_RETENTION", 24*time.Hour), "How long finished jobs stay listed (MODEL_MANAGER_JOB_RETENTION)")
	return cmd
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()

	backend.Register(backend.NameOllama, ollama.Factory)
	b, err := backend.New(backend.Name(o.backendName), backend.Options{
		Ollama: backend.OllamaOptions{Endpoint: o.ollamaEndpoint, AgentHost: o.ollamaAgentHost},
	})
	if err != nil {
		return err
	}
	log.Info("backend ready", "backend", b.Name(), "capabilities", b.Capabilities())

	var (
		wirer      wiring.Wirer
		wiringInfo *service.WiringInfo
	)
	if o.wiringDisabled {
		log.Info("agent wiring disabled by flag")
	} else {
		clients, err := kube.New(kube.Config{Kubeconfig: o.kubeconfig, Context: o.kubeContext, InCluster: o.inCluster})
		if err != nil {
			log.Warn("agent wiring disabled: no Kubernetes access", "error", err)
		} else {
			apiVersion := o.kagentAPIVersion
			if apiVersion == "" || apiVersion == "auto" {
				apiVersion, err = wiring.DiscoverAPIVersion(clients.Discovery)
				if err != nil {
					log.Warn("kagent API discovery failed, using default", "default", wiring.DefaultAPIVersion, "error", err)
					apiVersion = wiring.DefaultAPIVersion
				}
			}
			k := wiring.NewKagent(clients.Dynamic, o.kagentNamespace, apiVersion, o.modelConfigPrefix, b.Name())
			wirer = k
			wiringInfo = &service.WiringInfo{Namespace: k.Namespace(), APIVersion: k.APIVersion()}
			log.Info("agent wiring enabled", "namespace", k.Namespace(), "apiVersion", k.APIVersion(), "autoWire", o.autoWire)
		}
	}

	jm := jobs.NewManager(jobs.WithRetention(o.jobRetention))
	svc := service.New(b, jm, wirer, wiringInfo, service.Config{AutoWire: o.autoWire, DefaultKeepAlive: o.defaultKeepAlive}, log)

	cfg := server.Config{Addr: o.listen, MCPEnabled: o.mcpEnabled, MCPPath: o.mcpPath}
	if o.oauthEnabled {
		cfg.OAuth = &server.OAuthConfig{
			BaseURL:         o.oauthBaseURL,
			DexIssuerURL:    o.dexIssuerURL,
			DexClientID:     o.dexClientID,
			DexClientSecret: o.dexClientSecret,
		}
	}
	srv, err := server.New(cfg, svc, api.NewMCPServer(svc, version), log)
	if err != nil {
		return err
	}
	log.Info("model-manager starting", "version", version, "listen", o.listen, "rest", api.Prefix, "mcp", o.mcpPath, "mcpEnabled", o.mcpEnabled, "oauth", o.oauthEnabled)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		return err
	}
	jm.Wait()
	return nil
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

var _ = fmt.Sprintf
