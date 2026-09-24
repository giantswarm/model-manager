package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/registry"
)

// Backend registration tools: add_backend / remove_backend write a backend
// document (docs/backends.md) as the caller. Both take dryRun and mode; mode
// apply is the only accepted value until the commit path (a pull request
// through the broker grant) lands.
const (
	ToolAddBackend    = "add_backend"
	ToolRemoveBackend = "remove_backend"

	argKind               = "kind"
	argEndpoint           = "endpoint"
	argAgentEndpoint      = "agentEndpoint"
	argSource             = "source"
	argCredentialsSecret  = "credentialsSecret"
	argCredentialsKey     = "credentialsKey"
	argCluster            = "cluster"
	argOrganization       = "organization"
	argAPIServer          = "apiServer"
	argCABundle           = "caBundle"
	argServingNamespace   = "servingNamespace"
	argDiscoveryNamespace = "discoveryNamespace"
	argDiscoveryName      = "discoveryName"
	argGPUPoolTaint       = "gpuPoolTaint"
	argGPUPoolSelector    = "gpuPoolNodeSelector"
	argDryRun             = "dryRun"
	argMode               = "mode"

	// ModeApply writes the document directly; ModeCommit is reserved.
	ModeApply  = "apply"
	ModeCommit = "commit"

	// registrationWait bounds how long add/remove wait for the watch to
	// deliver the write before answering with the current state.
	registrationWait = 5 * time.Second
)

// Option tunes NewMCPServer.
type Option func(*tools)

// WithBackendStore enables add_backend / remove_backend over store; without
// it both tools answer that registration needs Kubernetes access.
func WithBackendStore(store *registry.Store) Option {
	return func(t *tools) { t.store = store }
}

func (t *tools) registerBackendTools(s *mcpserver.MCPServer) {
	s.AddTool(mcp.NewTool(ToolAddBackend,
		mcp.WithDescription("Register a model backend at runtime by writing its backend document — a ConfigMap in model-manager's namespace found by label — as you. One backend per kind: an existing document of the kind is replaced; a kind configured statically by the chart is refused. Host backends (ollama|lmstudio|lemonade) take endpoint and optionally agentEndpoint; kserve takes the target cluster (cluster `local` for this cluster, else apiServer and caBundle — never credentials), servingNamespace and where the discovery ConfigMap lives. dryRun returns the rendered document without writing; mode apply writes it (commit is not available yet)."),
		mcp.WithString(argKind, mcp.Required(), mcp.Description("Backend kind: ollama|lmstudio|lemonade|kserve")),
		mcp.WithString(argEndpoint, mcp.Description("Host backend base URL as reached by model-manager (ollama, lmstudio, lemonade)")),
		mcp.WithString(argAgentEndpoint, mcp.Description("Host backend base URL as reached by agent pods (default: endpoint)")),
		mcp.WithString(argSource, mcp.Description("Who registers it: person (default) or cluster-manager")),
		mcp.WithString(argCredentialsSecret, mcp.Description("kserve: Secret in the serving namespace holding the Hugging Face token (optional)")),
		mcp.WithString(argCredentialsKey, mcp.Description("Key in that Secret (default token)")),
		mcp.WithString(argCluster, mcp.Description("kserve: target cluster name; local (default) is the cluster model-manager runs on")),
		mcp.WithString(argOrganization, mcp.Description("kserve: the target cluster's organization")),
		mcp.WithString(argAPIServer, mcp.Description("kserve: the target apiserver URL (required unless cluster is local)")),
		mcp.WithString(argCABundle, mcp.Description("kserve: the target apiserver's CA bundle, PEM (required unless cluster is local)")),
		mcp.WithString(argServingNamespace, mcp.Description("kserve: the serving namespace on the target (required)")),
		mcp.WithString(argDiscoveryNamespace, mcp.Description("kserve: namespace of the model-serving discovery ConfigMap (default: the serving namespace)")),
		mcp.WithString(argDiscoveryName, mcp.Description("kserve: name of the discovery ConfigMap (default agent-platform-model-serving)")),
		mcp.WithString(argGPUPoolTaint, mcp.Description("kserve: the GPU node pool's taint as key[=value][:effect] (nvidia.com/gpu:NoSchedule), tolerated by the inventory scan pods, download Jobs and predictors; replaces the discovery ConfigMap's gpuPool.taint")),
		mcp.WithString(argGPUPoolSelector, mcp.Description("kserve: the GPU node pool's label as key=value[,key=value] (giantswarm.io/machine-pool=<cluster>-<pool>), the node selector of everything scheduled onto the pool; replaces the discovery ConfigMap's gpuPool.nodeSelector")),
		mcp.WithBoolean(argDryRun, mcp.Description("Render the document and the ConfigMap without writing (default false)")),
		mcp.WithString(argMode, mcp.Description("apply (default): write the ConfigMap as you. commit is not available yet")),
		mcp.WithIdempotentHintAnnotation(true),
	), t.addBackend)

	s.AddTool(mcp.NewTool(ToolRemoveBackend,
		mcp.WithDescription("Remove a backend registered at runtime: drops model-manager's ModelConfigs for it, then deletes its backend document; the backend disappears from list_backends. A static backend (chart values) cannot be removed here. dryRun reports what would go."),
		mcp.WithString(argKind, mcp.Required(), mcp.Description("Backend kind: ollama|lmstudio|lemonade|kserve")),
		mcp.WithBoolean(argDryRun, mcp.Description("Report the ConfigMap and ModelConfigs that would be removed (default false)")),
		mcp.WithString(argMode, mcp.Description("apply (default): delete as you. commit is not available yet")),
		mcp.WithDestructiveHintAnnotation(true),
	), t.removeBackend)
}

// mode validates the mode argument: apply, or a refusal naming the fix.
func mode(req mcp.CallToolRequest) error {
	switch m := strings.TrimSpace(req.GetString(argMode, ModeApply)); m {
	case ModeApply:
		return nil
	case ModeCommit:
		return fmt.Errorf("%w: mode commit (a pull request through the broker grant) is not available yet; use mode apply", backend.ErrUnsupported)
	default:
		return fmt.Errorf("%w: mode must be %s (commit is not available yet), got %q", backend.ErrInvalid, ModeApply, m)
	}
}

func (t *tools) documentFrom(req mcp.CallToolRequest) (*backend.Document, error) {
	get := func(k string) string { return strings.TrimSpace(req.GetString(k, "")) }
	spec := backend.DocumentSpec{
		Kind:          backend.Name(get(argKind)),
		Source:        get(argSource),
		Endpoint:      get(argEndpoint),
		AgentEndpoint: get(argAgentEndpoint),
	}
	if name := get(argCredentialsSecret); name != "" {
		spec.Credentials = &backend.CredentialsRef{SecretRef: backend.SecretKeyRef{Name: name, Key: get(argCredentialsKey)}}
	}
	if spec.Kind == backend.NameKServe {
		spec.KServe = &backend.KServeSpec{
			Target: backend.Target{
				Cluster:          get(argCluster),
				Organization:     get(argOrganization),
				APIServer:        get(argAPIServer),
				CABundle:         get(argCABundle),
				ServingNamespace: get(argServingNamespace),
			},
			Discovery: backend.DiscoveryRef{Namespace: get(argDiscoveryNamespace), Name: get(argDiscoveryName)},
		}
		pool, err := gpuPoolFrom(get(argGPUPoolTaint), get(argGPUPoolSelector))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", backend.ErrInvalid, err)
		}
		spec.KServe.GPUPool = pool
	}
	doc := backend.NewDocument(spec)
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", backend.ErrInvalid, err)
	}
	return doc, nil
}

// gpuPoolFrom parses the GPU pool arguments: the taint as key[=value][:effect]
// (kubectl taint's notation), the selector as key=value[,key=value]. Nil when
// both are empty.
func gpuPoolFrom(taint, selector string) (*backend.GPUPool, error) {
	if taint == "" && selector == "" {
		return nil, nil
	}
	pool := &backend.GPUPool{}
	if taint != "" {
		kv, effect, _ := strings.Cut(taint, ":")
		key, value, _ := strings.Cut(kv, "=")
		if key == "" {
			return nil, fmt.Errorf("%s: must be key[=value][:effect], got %q", argGPUPoolTaint, taint)
		}
		pool.Taint = &backend.Taint{Key: key, Value: value, Effect: effect}
	}
	if selector != "" {
		pool.NodeSelector = map[string]string{}
		for _, pair := range strings.Split(selector, ",") {
			key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok || key == "" {
				return nil, fmt.Errorf("%s: must be key=value[,key=value], got %q", argGPUPoolSelector, selector)
			}
			pool.NodeSelector[key] = value
		}
	}
	return pool, nil
}

func (t *tools) noStore() *mcp.CallToolResult {
	return errResult(fmt.Errorf("%w: backend registration needs Kubernetes access (run in-cluster or with a kubeconfig, and set --namespace)", backend.ErrUnsupported))
}

func (t *tools) addBackend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mode(req); err != nil {
		return errResult(err), nil
	}
	doc, err := t.documentFrom(req)
	if err != nil {
		return errResult(err), nil
	}
	if src, has := t.svc.Has(doc.Spec.Kind); has && src == backend.SourceStatic {
		return errResult(fmt.Errorf("%w: backend %s is configured statically by the chart values (--backends); remove it there to register it at runtime", backend.ErrConflict, doc.Spec.Kind)), nil
	}
	if t.store == nil {
		return t.noStore(), nil
	}
	rendered, err := doc.Render()
	if err != nil {
		return errResult(err), nil
	}
	cm, err := doc.ConfigMap(t.store.Namespace())
	if err != nil {
		return errResult(err), nil
	}
	out := map[string]any{
		"dryRun":    req.GetBool(argDryRun, false),
		"document":  string(rendered),
		"configMap": map[string]any{"namespace": cm.Namespace, "name": cm.Name, "labels": cm.Labels},
	}
	if req.GetBool(argDryRun, false) {
		return jsonResult(out)
	}
	created, err := t.store.Apply(ctx, doc)
	if err != nil {
		return errResult(err), nil
	}
	out["created"] = created
	registered := registry.WaitFor(ctx, t.svc, doc.Spec.Kind, true, registrationWait)
	out["registered"] = registered
	if !registered {
		// Refused when it was read (a remote target without downstream
		// OAuth, the daemonset inventory there): the answer says why. A
		// registered document edited into a refusal is dropped and reported
		// by the registry in one step; list_backends shows it.
		for _, d := range t.svc.InvalidDocuments() {
			if d.ConfigMap == cm.Name {
				out["error"] = d.Error
			}
		}
	}
	if b, err := t.svc.Backend(ctx, string(doc.Spec.Kind)); err == nil {
		out["backend"] = b
	}
	return jsonResult(out)
}

func (t *tools) removeBackend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mode(req); err != nil {
		return errResult(err), nil
	}
	kind := backend.Name(strings.TrimSpace(req.GetString(argKind, "")))
	if kind == "" {
		return errResult(fmt.Errorf("%w: kind is required", backend.ErrInvalid)), nil
	}
	if src, has := t.svc.Has(kind); has && src == backend.SourceStatic {
		return errResult(fmt.Errorf("%w: backend %s is configured statically by the chart values (--backends) and cannot be removed here", backend.ErrConflict, kind)), nil
	}
	if t.store == nil {
		return t.noStore(), nil
	}
	dryRun := req.GetBool(argDryRun, false)
	out := map[string]any{
		"dryRun":    dryRun,
		"configMap": map[string]any{"namespace": t.store.Namespace(), "name": backend.DocumentName(kind)},
	}
	if dryRun {
		if _, err := t.store.Get(ctx, kind); err != nil {
			return errResult(err), nil
		}
		wired, err := t.svc.WiredModels(ctx, kind)
		if err != nil {
			return errResult(err), nil
		}
		out["modelConfigs"] = wired
		return jsonResult(out)
	}
	unwired, err := t.svc.UnwireBackend(ctx, kind)
	if err != nil {
		return errResult(err), nil
	}
	out["unwired"] = unwired
	found, err := t.store.Remove(ctx, kind)
	if err != nil {
		return errResult(err), nil
	}
	out["removed"] = found
	if !found {
		return errResult(fmt.Errorf("%w: no %s backend document in %s", backend.ErrNotFound, kind, t.store.Namespace())), nil
	}
	out["deregistered"] = registry.WaitFor(ctx, t.svc, kind, false, registrationWait)
	return jsonResult(out)
}
