// Package kserve implements the serving backend over KServe's llm-d control
// plane: the inventory is the per-node Hugging Face cache (a
// PersistentVolumeClaim scanned by a short-lived pod) plus the
// LLMInferenceServices of the serving namespace; pull is a pre-warm download
// Job into that cache; load composes an LLMInferenceService from a curated
// serving preset (the modelServing contract of the agent-platform
// connectivity chart); unload deletes it. Sizes come from the Hugging
// Face Hub and are fit-checked against node memory budgets before any download
// or start. Agents reach a served model through kagent's OpenAI provider: a
// model routed on the models Gateway with the caller's own token forwarded
// (the Gateway admits nothing else), a model reached on its in-cluster
// Service with a placeholder API key.
package kserve

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/model-manager/internal/backend"
)

// logReader reads a pod's log with the options given — the container, a
// tail, the previous instance (replaced in tests: the fake clientset returns
// a fixed body).
type logReader func(ctx context.Context, namespace, name string, opts corev1.PodLogOptions) (string, error)

// Backend is the kserve driver.
type Backend struct {
	opts backend.KServeOptions
	cfg  *config
	cs   kubernetes.Interface
	dyn  dynamic.Interface
	hub  *hubClient
	inv  *inventory
	log  *slog.Logger

	// callerlessLog: the first detached work skipped for want of a caller
	// on a remote target is logged (target.go).
	callerlessLog sync.Once

	// scan and logs are the node-touching primitives; tests replace them.
	scan scanner
	// liveCache says scans reach the cache without creating a pod (the
	// cache-agent daemonset), so a list call may read a filling directory.
	liveCache bool
	logs      logReader
	// agentHTTP talks to the cache-agent pods (daemonset inventory mode);
	// serverHTTP to the served models' runtimes (interfaces.go).
	agentHTTP  *http.Client
	serverHTTP *http.Client
	// targetREST is the caller's REST client toward a remote target, whose
	// Service proxy reaches the runtimes there (origin.go).
	targetREST func(context.Context) *rest.RESTClient

	// index is the driver's copy of the cache index ConfigMap (index.go).
	index cacheIndex

	mu          sync.Mutex
	servedCache []served
	presetCache []*servingPreset
	token       string
	tokenAt     time.Time

	// apiMu guards apiCache: what each Ready transition's server read found.
	apiMu    sync.Mutex
	apiCache map[string]serverAPI
	// askMu guards asks: each object's first request by uid (answer.go).
	askMu sync.Mutex
	asks  map[string]*firstAsk
	// gwMu guards what the last write of the LLM endpoint's
	// AgentgatewayModels found and when (gatewaymodel.go).
	gwMu          sync.Mutex
	gwOnEndpoint  gatewayState
	gwFingerprint string
	gwSyncedAt    time.Time
}

// k8s returns the typed client a call should use: the caller's own when ctx
// carries a caller token (downstream OAuth), else the ServiceAccount's.
func (b *Backend) k8s(ctx context.Context) kubernetes.Interface {
	if b.opts.ClientsFor != nil {
		if cs, _ := b.opts.ClientsFor(ctx); cs != nil {
			return cs
		}
	}
	return b.cs
}

// dynamic is k8s for the dynamic client (LLMInferenceServices).
func (b *Backend) dynamic(ctx context.Context) dynamic.Interface {
	if b.opts.ClientsFor != nil {
		if _, dyn := b.opts.ClientsFor(ctx); dyn != nil {
			return dyn
		}
	}
	return b.dyn
}

// Factory builds the driver from backend.Options.
func Factory(opts backend.Options) (backend.Backend, error) {
	return New(opts.KServe)
}

// New builds the driver.
func New(opts backend.KServeOptions) (*Backend, error) {
	if opts.Clientset == nil || opts.Dynamic == nil {
		return nil, fmt.Errorf("kserve backend needs Kubernetes access")
	}
	applyDefaults(&opts)
	switch opts.BudgetSource {
	case budgetSourceAuto, budgetSourceGPULabels, budgetSourceAllocatable:
	default:
		return nil, fmt.Errorf("kserve budget source %q: want auto, gpu-labels or allocatable", opts.BudgetSource)
	}
	switch opts.InventoryMode {
	case InventoryModePod:
	case InventoryModeDaemonSet:
		if !opts.Target.Local() {
			return nil, errDaemonSetRemote(opts.Target)
		}
	default:
		return nil, fmt.Errorf("kserve inventory mode %q: want %s or %s", opts.InventoryMode, InventoryModePod, InventoryModeDaemonSet)
	}
	log := slog.Default().With("backend", "kserve")
	b := &Backend{
		opts: opts,
		cfg:  newConfig(opts, log),
		cs:   opts.Clientset,
		dyn:  opts.Dynamic,
		inv:  newInventory(),
		log:  log,
	}
	// The client's timeout is the ceiling of one exchange; the interactive
	// lookups (fit check, search) are bounded by opts.HFTimeout on their
	// context, so a hub that does not answer cannot outlive the caller.
	b.hub = newHubClient(opts.HFEndpoint, &http.Client{Timeout: 30 * time.Second}, b.hubToken)
	b.agentHTTP = &http.Client{Timeout: opts.InventoryTimeout}
	b.serverHTTP = &http.Client{Timeout: serverReadTimeout}
	b.targetREST = b.callerREST
	b.scan = b.scanNode
	if opts.InventoryMode == InventoryModeDaemonSet {
		b.scan = b.scanAgent
		b.liveCache = true
	}
	return b, nil
}

// Name implements backend.Backend.
func (b *Backend) Name() backend.Name { return backend.NameKServe }

// Capabilities implements backend.Backend. Wire is decided by the service.
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		Pull:          true,
		PullProgress:  true,
		Delete:        true,
		Load:          true,
		Unload:        true,
		LoadedModels:  true,
		Presets:       true,
		FitCheck:      true,
		NodeInventory: true,
		Search:        true,
	}
}

// Info implements backend.Backend: healthy when the LLMInferenceService API
// answers in the serving namespace. The message says what keeps a load from
// being served — the llm-d controller not installed, the API not served —
// before anyone tries one (giantswarm/model-manager#129), else what the
// discovery document is up to.
func (b *Backend) Info(ctx context.Context) backend.Info {
	s := b.cfg.settings(ctx)
	info := backend.Info{
		Backend:  backend.NameKServe,
		Version:  llmisvcGVR.GroupVersion().String(),
		Endpoint: fmt.Sprintf("%s.%s/%s", llmisvcGVR.Resource, llmisvcGVR.Group, s.Namespace),
		// An LLMInferenceService serves only while it exists: nothing loads
		// a stopped model on request and nothing evicts a running one.
		Loading: backend.Loading{OnDemand: false, IdleEviction: false},
		Target:  b.Target(),
		GPUPool: s.gpuPoolReport(),
		// The models Gateway's origin is the host every ModelConfig names
		// when the discovery document enables it — on a remote target the
		// one way agents on the installation reach a model served there.
		AgentEndpoint: s.GatewayEndpoint,
	}
	if _, err := b.dynamic(ctx).Resource(llmisvcGVR).Namespace(s.Namespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		info.Message = fmt.Sprintf("%s API not available in %s: %v", kindLLMInferenceService, s.Namespace, err)
		return info
	}
	info.Healthy = true
	switch {
	case s.servingUnavailable() != "":
		info.Message = s.servingUnavailable()
	case s.DiscoveryError != "":
		info.Message = "discovery: " + s.DiscoveryError
	case !s.DiscoveryFound:
		info.Message = fmt.Sprintf("no discovery ConfigMap %s/%s; using flags and defaults", b.opts.DiscoveryNamespace, b.opts.DiscoveryConfigMap)
	case s.GatewayEndpoint == "" && !b.opts.Target.Local():
		info.Message = fmt.Sprintf("the discovery document on %s enables no models Gateway (spec.gateway): a model served there gets a cluster-local address that agents on the installation cannot reach", b.opts.Target.Cluster)
	}
	return info
}

// ListModels implements backend.Backend: the cache contents per node plus the
// served models whose weights are not cached.
func (b *Backend) ListModels(ctx context.Context) ([]backend.Model, error) {
	presets, _, err := b.presets(ctx)
	if err != nil {
		return nil, err
	}
	idx := indexPresets(presets)
	servedList, err := b.listServed(ctx)
	if err != nil {
		return nil, err
	}
	servedByName := map[string]served{}
	for _, sv := range servedList {
		servedByName[sv.Name] = sv
	}
	entries, err := b.cacheEntries(ctx)
	if err != nil {
		return nil, err
	}
	index := b.cacheIndexWith(ctx, servedList)

	out := make([]backend.Model, 0, len(entries)+len(servedList))
	seen := map[string]bool{} // node/dir
	for _, e := range entries {
		if !e.holdsModel() {
			// Hugging Face client internals (hf-home, xet) or a directory
			// still filling up: not a download. A served model whose
			// directory is still filling is listed below as not downloaded.
			continue
		}
		m := b.modelFromEntry(e, idx, servedByName, index)
		seen[e.Node+"/"+e.Dir] = true
		out = append(out, m)
	}
	for _, sv := range servedList {
		if seen[sv.Node+"/"+sv.Name] || (sv.Node == "" && anyDir(seen, sv.Name)) {
			continue
		}
		m := backend.Model{
			Name:       sv.Model,
			Node:       sv.Node,
			Path:       sv.Name,
			Preset:     sv.Preset,
			Downloaded: ptr.To(false),
			ModifiedAt: sv.Created,
		}
		if p, ok := idx.byName[sv.Preset]; ok {
			m.SizeBytes = p.weightsBytes()
			m.Format = p.Spec.Model.Format
			m.ContextLength = p.Spec.Model.ContextLength
			m.Capabilities = p.Spec.Model.Capabilities
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Node < out[j].Node
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func anyDir(seen map[string]bool, dir string) bool {
	for k := range seen {
		if strings.HasSuffix(k, "/"+dir) {
			return true
		}
	}
	return false
}

// modelFromEntry names a cache directory by what is known to have filled it:
// the marker a pre-warm download wrote, the cache index record of the
// LLMInferenceService that served from it (index.go — live objects are part
// of the index), then the assumptions — the preset of the same name, the
// LLMInferenceService of the same name — else the directory itself. The preset is
// the one of the same name, else the one the record names when it still serves
// that model, else the single preset serving the model.
func (b *Backend) modelFromEntry(e cacheEntry, idx presetIndex, servedByName map[string]served, index map[string]indexEntry) backend.Model {
	m := backend.Model{
		Node:       e.Node,
		Path:       e.Dir,
		SizeBytes:  e.Bytes,
		ModifiedAt: e.MTime,
		Downloaded: ptr.To(e.Files > 0),
	}
	record, remembered := index[e.Dir]
	switch {
	case e.Marker != nil && e.Marker.Model != "":
		m.Name, m.Digest = e.Marker.Model, e.Marker.Revision
	case remembered:
		m.Name, m.Digest = record.Model, record.Revision
	case idx.byName[e.Dir] != nil:
		m.Name = idx.byName[e.Dir].Spec.Model.ID
	case servedByName[e.Dir].Model != "":
		m.Name = servedByName[e.Dir].Model
	default:
		m.Name = e.Dir
	}
	switch {
	case idx.byName[e.Dir] != nil:
		m.Preset = e.Dir
	case remembered && record.Preset != "" && presetServes(idx, record.Preset, m.Name):
		m.Preset = record.Preset
	default:
		if matches := idx.forModel(m.Name); len(matches) == 1 {
			m.Preset = matches[0].name()
		}
	}
	if p, ok := idx.byName[m.Preset]; ok {
		m.Format = p.Spec.Model.Format
		m.ContextLength = p.Spec.Model.ContextLength
		m.Capabilities = p.Spec.Model.Capabilities
	}
	return m
}

// presetServes reports whether the named preset exists and serves the model.
func presetServes(idx presetIndex, preset, model string) bool {
	p, ok := idx.byName[preset]
	return ok && strings.EqualFold(p.Spec.Model.ID, model)
}

// countModels is the number of cache directories that hold a model.
func countModels(entries []cacheEntry) int {
	n := 0
	for _, e := range entries {
		if e.holdsModel() {
			n++
		}
	}
	return n
}

// cacheEntries scans every cache node (or the shared cache once).
func (b *Backend) cacheEntries(ctx context.Context) ([]cacheEntry, error) {
	s := b.cfg.settings(ctx)
	if !s.CacheEnabled {
		return nil, nil
	}
	loc, err := b.cacheNodes(ctx)
	if err != nil {
		return nil, err
	}
	if loc.Missing {
		b.log.Debug("cache claim missing; inventory lists served models only", "claim", loc.Claim)
		return nil, nil
	}
	nodes := loc.Nodes
	if len(nodes) == 0 {
		// Shared storage: one scan, wherever the scheduler puts it — once
		// scanAllowed permits one (not on an unbound claim, not while the
		// GPU pool has no node).
		nodes = []string{""}
	}
	var out []cacheEntry
	for _, node := range nodes {
		snap := b.cacheSnapshotFor(ctx, node, loc)
		if snap.Err != nil {
			b.log.Warn("cache scan failed", "node", nodeOrAny(node), "error", snap.Err)
		}
		if snap.Pending {
			b.log.Debug("cache scan deferred", "node", nodeOrAny(node), "reason", snap.PendingReason)
		}
		for _, e := range snap.Entries {
			if e.Node == "" {
				e.Node = snap.Node
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// GetModel implements backend.Backend. Names resolve as repository id, cache
// directory or preset name; a model only known from a preset is returned with
// downloaded=false so it can be loaded (the LLMInferenceService downloads).
func (b *Backend) GetModel(ctx context.Context, name string) (*backend.Model, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	repo, _ := splitRevision(name)
	models, err := b.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range models {
		m := models[i]
		if strings.EqualFold(m.Name, repo) || m.Path == name || (m.Preset != "" && m.Preset == name) {
			return &m, nil
		}
	}
	presets, _, err := b.presets(ctx)
	if err != nil {
		return nil, err
	}
	idx := indexPresets(presets)
	p, err := idx.resolve(repo, "")
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%w: %s is neither cached, served nor a preset", backend.ErrNotFound, name)
	}
	return &backend.Model{
		Name:          p.Spec.Model.ID,
		SizeBytes:     p.weightsBytes(),
		Format:        p.Spec.Model.Format,
		ContextLength: p.Spec.Model.ContextLength,
		Capabilities:  p.Spec.Model.Capabilities,
		Preset:        p.name(),
		Path:          p.name(),
		Downloaded:    ptr.To(false),
	}, nil
}

// ListLoaded implements backend.Backend: the LLMInferenceServices.
func (b *Backend) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	servedList, err := b.listServed(ctx)
	if err != nil {
		return nil, err
	}
	presets, _, err := b.presets(ctx)
	if err != nil {
		return nil, err
	}
	idx := indexPresets(presets)
	out := make([]backend.LoadedModel, 0, len(servedList))
	for _, sv := range servedList {
		lm := backend.LoadedModel{
			Name:      sv.Model,
			Endpoint:  sv.URL,
			Node:      sv.Node,
			Pool:      sv.Pool,
			Status:    sv.Status,
			Reason:    sv.Reason,
			Message:   sv.Message,
			Resource:  sv.Name,
			Kind:      kindLLMInferenceService,
			Preset:    sv.Preset,
			GPUs:      sv.GPUs,
			ManagedBy: sv.ManagedBy,
			Phase:     sv.Phase,
			Steps:     sv.Steps,
		}
		if sv.API.read() {
			lm.Runtime, lm.Interfaces, lm.InterfacesReason = sv.API.Runtime, sv.API.Interfaces, sv.API.Reason
		}
		if sv.LLM != nil && !sv.Deleting {
			if sv.OnEndpoint {
				lm.PublicName, lm.Endpoint = sv.onEndpointName(), sv.LLM.clientURL()
			} else {
				lm.PublicNameReason = sv.EndpointReason
			}
		}
		if sv.Deleting {
			lm.Status = statusTerminating
		}
		if p, ok := idx.byName[sv.Preset]; ok {
			lm.SizeBytes = p.weightsBytes()
			lm.ContextLength = p.Spec.Model.ContextLength
		}
		out = append(out, lm)
	}
	return out, nil
}

// Pull implements backend.Backend: fit-check, then a download Job into the
// cache directory the model's LLMInferenceService will mount. A preset served
// from an OCI model image has nothing to pull into the cache — the nodes pull
// the image themselves — and is refused as invalid before the hub is asked
// (giantswarm/model-manager#123).
func (b *Backend) Pull(ctx context.Context, req backend.PullRequest, progress func(backend.Progress)) error {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return fmt.Errorf("%w: empty model reference", backend.ErrInvalid)
	}
	fitReq := backend.FitRequest{Model: ref, Preset: req.Preset, Node: req.Node}
	plan, idx, err := b.resolveFit(ctx, fitReq)
	if err != nil {
		return err
	}
	if p := plan.Preset; !p.storesInCache() {
		return fmt.Errorf("%w: %s is served from an OCI model image the platform pre-pulls on its GPU nodes (preset %s, %s); nothing to download", backend.ErrInvalid, plan.Repo, p.name(), p.Spec.Model.StorageURI)
	}
	if err := b.judgeFit(ctx, plan, idx, fitReq, false); err != nil {
		return err
	}
	res := plan.Result
	if !res.Fits {
		return fmt.Errorf("%w: %s", backend.ErrUnfit, res.Reason)
	}
	if res.Gated && !res.TokenConfigured {
		return fmt.Errorf("%w: %s is gated; configure a Hugging Face token (kserve.hf.tokenSecret) to download it", backend.ErrInvalid, plan.Repo)
	}
	dl := downloadPlan{Repo: plan.Repo, Revision: plan.Revision, Dir: plan.Dir, Preset: res.Preset, BytesTotal: res.DownloadBytes}
	if plan.CacheLocal || req.Node != "" {
		dl.Node = res.Node
	}
	if res.Cached {
		if progress != nil {
			progress(backend.Progress{Status: "already cached", BytesCompleted: res.DownloadBytes, BytesTotal: res.DownloadBytes, Node: dl.Node, Preset: dl.Preset})
		}
		return nil
	}
	job, adopted, err := b.ensureJob(ctx, dl)
	if err != nil {
		return err
	}
	b.log.Info("download job", "job", job.Name, "model", plan.Repo, "dir", plan.Dir, "node", nodeOrAny(dl.Node), "adopted", adopted, "bytesTotal", res.DownloadBytes)
	return b.watchJob(ctx, dl, progress)
}

// Delete implements backend.Backend: removes the cache directory. A model
// that is being served must be unloaded first.
func (b *Backend) Delete(ctx context.Context, name string) error {
	m, err := b.GetModel(ctx, name)
	if err != nil {
		return err
	}
	if m.Downloaded == nil || !*m.Downloaded {
		return fmt.Errorf("%w: %s is not in the cache", backend.ErrNotFound, name)
	}
	servedList, err := b.listServed(ctx)
	if err != nil {
		return err
	}
	for _, sv := range servedList {
		if sv.Name == m.Path || strings.EqualFold(sv.Model, m.Name) {
			return fmt.Errorf("%w: %s is served by %s %s; unload it first", backend.ErrConflict, m.Name, kindLLMInferenceService, sv.Name)
		}
	}
	loc, err := b.cacheNodes(ctx)
	if err != nil {
		return err
	}
	node := m.Node
	if loc.Shared {
		node = ""
	}
	if err := b.removeDir(ctx, node, m.Path); err != nil {
		return err
	}
	b.forgetDir(ctx, m.Path)
	b.inv.invalidate()
	return nil
}

// Load implements backend.Backend: fit-check, then create the
// LLMInferenceService composed from the preset. Loading the same preset again
// is a no-op.
func (b *Backend) Load(ctx context.Context, req backend.LoadRequest) error {
	_, err := b.Serve(ctx, req)
	return err
}

// Serve implements backend.Server: Load with the fit verdict the model was
// judged by in the answer (giantswarm/model-manager#110). It fails fast, before
// the fit check and with nothing created, where no llm-d control plane would
// reconcile the object (settings.servingUnavailable) — judged on the cluster
// as it is now, not on a verdict cached before the serving slice's runtime
// configs landed (config.settingsForLoad).
func (b *Backend) Serve(ctx context.Context, req backend.LoadRequest) (*backend.LoadResult, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" && req.Preset == "" {
		return nil, fmt.Errorf("%w: model or preset is required", backend.ErrInvalid)
	}
	s := b.cfg.settingsForLoad(ctx)
	if reason := s.servingUnavailable(); reason != "" {
		return nil, fmt.Errorf("%w: %s", backend.ErrUnavailable, reason)
	}
	plan, err := b.fitCheck(ctx, backend.FitRequest{Model: name, Preset: req.Preset, Node: req.Node})
	if err != nil {
		return nil, err
	}
	if plan.Preset == nil {
		return nil, fmt.Errorf("%w: no serving preset serves %s; presets are curated in the platform chart (components.modelServing.presets)", backend.ErrInvalid, plan.Repo)
	}
	fit := plan.Result
	fit.Backend = b.Name()
	res := &backend.LoadResult{Fit: &fit}
	// The object is composed with the settings the fit was judged on: a fit
	// that found no node re-reads a discovery document that was absent or
	// did not name the GPU pool yet (config.recheckDiscovery), and the pool
	// it placed the model on is the one the predictor has to be scheduled
	// onto (giantswarm/model-manager#127). A CPU preset is composed without
	// the GPU pool's scheduling and the accelerator RuntimeClass
	// (settings.forPreset).
	s = b.cfg.settings(ctx).forPreset(plan.Preset)
	if fit.Pool != "" && len(s.GPUPool.NodeSelector) == 0 {
		// Several pools and none pinning every predictor: this one is pinned
		// to the pool the fit placed the model on (giantswarm/model-manager#152).
		s.GPUPool.NodeSelector = map[string]string{labelMachinePool: fit.Pool}
	}
	existing, err := b.getServing(ctx, s.Namespace, plan.Preset.name())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		sv := parseServed(existing, indexPresets([]*servingPreset{plan.Preset}), s)
		if sv.manageable() && strings.EqualFold(sv.Model, plan.Repo) {
			b.log.Info("serving object already exists", "name", sv.Name, "model", sv.Model, "managedBy", sv.ManagedBy)
			return res, nil
		}
		return nil, fmt.Errorf("%w: %s %s/%s exists (model %s, managed by %q)", backend.ErrConflict, kindLLMInferenceService, s.Namespace, sv.Name, sv.Model, sv.ManagedBy)
	}
	if !plan.Result.Fits {
		return nil, fmt.Errorf("%w: %s", backend.ErrUnfit, plan.Result.Reason)
	}
	obj := b.composeLLM(plan.Preset, s, req.Node)
	if err := b.createServing(ctx, obj); err != nil {
		return nil, err
	}
	b.log.Info("serving object created", "name", obj.GetName(), "namespace", s.Namespace, "model", plan.Repo, "preset", plan.Preset.name(), "node", nodeOrAny(req.Node))
	b.inv.invalidate()
	return res, nil
}

// Unload implements backend.Backend: Stop without the answer.
func (b *Backend) Unload(ctx context.Context, name string) error {
	_, err := b.Stop(ctx, name)
	return err
}

// Stop implements backend.Stopper: deletes the LLMInferenceServices serving
// the model — found among the served objects by repository id, object name or
// preset name, never through the cache inventory — and answers what follows;
// the cache stays, and so does the index entry of a directory in it, while an
// entry that names a directory in no cache goes (forgetStale). model-manager's
// own objects and the ones the portal created from a preset are deleted;
// hand-written ones are not. The
// inventory is rescanned in the background where a scan pod may run
// (refreshInventory), so a caller under a short deadline gets the deletion
// done whatever the scan would take (giantswarm/model-manager#119).
func (b *Backend) Stop(ctx context.Context, name string) (*backend.UnloadResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	matches, err := b.servedFor(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: no %s serves %s", backend.ErrNotFound, kindLLMInferenceService, name)
	}
	for _, sv := range matches {
		if !sv.manageable() {
			return nil, fmt.Errorf("%w: %s %s/%s was not created from a serving preset (managed by %q); delete it where it was created", backend.ErrConflict, kindLLMInferenceService, sv.Namespace, sv.Name, sv.ManagedBy)
		}
	}
	for _, sv := range matches {
		if err := b.deleteServing(ctx, sv.Namespace, sv.Name); err != nil {
			return nil, err
		}
		b.log.Info("serving object deleted", "name", sv.Name, "namespace", sv.Namespace, "model", sv.Model)
		// Off the LLM endpoint in the same call: a client sending the public
		// name gets model_not_found, not a dead upstream.
		if name := sv.onEndpointName(); sv.LLM != nil && name != "" {
			if err := b.deleteGatewayModel(ctx, sv.LLM, name); err != nil {
				return nil, err
			}
		}
	}
	b.forgetStale(ctx, matches)
	return &backend.UnloadResult{Model: matches[0].Model, Inventory: b.refreshInventory(ctx)}, nil
}

// servedFor finds the LLMInferenceServices serving a model (by repository id,
// object name or preset name).
func (b *Backend) servedFor(ctx context.Context, name string) ([]served, error) {
	servedList, err := b.listServed(ctx)
	if err != nil {
		return nil, err
	}
	repo, _ := splitRevision(name)
	var out []served
	for _, sv := range servedList {
		if strings.EqualFold(sv.Model, repo) || sv.Name == name || (sv.Preset != "" && sv.Preset == name) {
			out = append(out, sv)
		}
	}
	return out, nil
}

// WaitReady implements backend.ServeLifecycle.
func (b *Backend) WaitReady(ctx context.Context, model string) error {
	ctx, cancel := context.WithTimeout(ctx, b.opts.ReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(b.opts.PollInterval)
	defer ticker.Stop()
	for {
		matches, err := b.servedFor(ctx, model)
		switch {
		case apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err):
			// The credential this wait runs with — a caller token that expired
			// while the predictor waited for a node — reads nothing anymore;
			// polling on would report the same every few seconds for hours.
			return fmt.Errorf("waiting for %s to become ready: %w; the serving object keeps starting — the loaded models list shows its state and wire_model wires it once Ready", model, err)
		case err != nil:
			b.log.Warn("readiness poll failed", "model", model, "error", err)
		case len(matches) == 0:
			return fmt.Errorf("%w: %s for %s is gone", backend.ErrNotFound, kindLLMInferenceService, model)
		default:
			for _, sv := range matches {
				if sv.Ready {
					return nil
				}
			}
			// A first request that failed stays failed until the model turns
			// Ready anew: waiting on would report the same until the timeout.
			for _, sv := range matches {
				if failure := sv.answerFailure(); failure != "" {
					return fmt.Errorf("%s is not ready: %s", model, failure)
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to become ready: %w", model, ctx.Err())
		case <-ticker.C:
		}
	}
}

// RunningPulls implements backend.PullAdopter.
func (b *Backend) RunningPulls(ctx context.Context) ([]backend.PullRequest, error) {
	return b.runningDownloads(ctx)
}

// AgentEndpoint implements backend.Backend: kagent's OpenAI provider against
// the model's OpenAI-compatible API at the address KServe published — or,
// until it does, the address the object is expected on: its route on the
// models Gateway when discovery names one, the in-cluster Service otherwise
// (served.expectedURL). A model routed on the Gateway is reached with the
// caller's Bearer token forwarded — the Gateway's JWT policy admits a
// person's token and nothing else, so a placeholder key fails every turn
// with 401; a model reached on its in-cluster Service needs the placeholder
// key kagent's OpenAI runtime insists on, which keyless vLLM never checks.
// Knowing the routed address at compose time is what lets a load wire the
// ModelConfig in the same call, before the model is ready
// (giantswarm/model-manager#115). vLLM serves the model under the
// LLMInferenceService's spec.model.name (the well-known template passes it).
// The ModelConfig is named after the object — the rule the portal's serve
// flow applies, so both wire a served model to the same ModelConfig.
func (b *Backend) AgentEndpoint(model string) backend.AgentEndpoint {
	repo, _ := splitRevision(model)
	b.mu.Lock()
	servedList := b.servedCache
	presets := b.presetCache
	b.mu.Unlock()
	for _, sv := range servedList {
		if strings.EqualFold(sv.Model, repo) || sv.Name == model {
			return sv.agentEndpoint()
		}
	}
	// Not served (yet): the object the preset would create, at the address
	// it will get.
	s := b.cfg.last()
	sv := served{Namespace: s.Namespace, Name: dnsLabel(repo), Model: repo, LLM: s.LLMEndpoint}
	if p, err := indexPresets(presets).resolve(repo, ""); err == nil && p != nil {
		// The object a load of the preset creates: model-manager's own.
		sv.Name, sv.Model, sv.Preset, sv.Managed = p.name(), p.Spec.Model.ID, p.name(), true
	}
	sv.URL = sv.expectedURL(s.GatewayEndpoint)
	return sv.agentEndpoint()
}

// ListPresets implements backend.PresetLister.
func (b *Backend) ListPresets(ctx context.Context) ([]backend.Preset, error) {
	presets, warnings, err := b.presets(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		b.log.Warn("skipping unusable preset", "detail", w)
	}
	out := make([]backend.Preset, 0, len(presets))
	for _, p := range presets {
		out = append(out, p.view(b.opts.DefaultOverheadGiB))
	}
	return out, nil
}

// Search implements backend.Searcher.
func (b *Backend) Search(ctx context.Context, query string, limit int) ([]backend.SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("%w: query is required", backend.ErrInvalid)
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	hctx, hubBudget, cancel := b.hubContext(ctx)
	defer cancel()
	hits, err := b.hub.Search(hctx, query, limit)
	if err != nil {
		return nil, hubFailure(err, hubBudget)
	}
	if presets, _, err := b.presets(ctx); err == nil {
		idx := indexPresets(presets)
		for i := range hits {
			for _, p := range idx.forModel(hits[i].ID) {
				hits[i].Presets = append(hits[i].Presets, p.name())
			}
		}
	}
	return hits, nil
}

// FitCheck implements backend.FitChecker.
func (b *Backend) FitCheck(ctx context.Context, req backend.FitRequest) (*backend.FitResult, error) {
	plan, err := b.fitCheck(ctx, req)
	if err != nil {
		return nil, err
	}
	res := plan.Result
	return &res, nil
}

// ListNodes implements backend.NodeLister: the accelerator nodes, each with
// its budget, reservation, cache and whether it is a serving target.
func (b *Backend) ListNodes(ctx context.Context) ([]backend.NodeInfo, error) {
	loc, err := b.cacheNodes(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := b.nodes(ctx, loc, nil)
	if err != nil {
		return nil, err
	}
	s := b.cfg.settings(ctx)
	presets, _, err := b.presets(ctx)
	if err != nil {
		return nil, err
	}
	reserved := b.reservedByNode(ctx, indexPresets(presets), nil)
	out := make([]backend.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		var cache *backend.NodeCache
		if s.CacheEnabled && !loc.Missing && (loc.Shared || containsString(loc.Nodes, n.Name) || (len(loc.Nodes) == 0 && !loc.Bound)) {
			scanNode := n.Name
			if loc.Shared || len(loc.Nodes) == 0 {
				scanNode = ""
			}
			snap := b.inv.snapshot(ctx, scanNode, b.opts.InventoryTTL, false, b.scan)
			if scanNode == "" && snap.Node != "" && snap.Node != n.Name && !loc.Shared {
				// The late-binding claim landed on another node.
				out = append(out, nodeView(n, reserved[n.Name], nil))
				continue
			}
			// Models counts directories that hold a model; bytes count everything
			// on the claim, client internals included — they occupy it too.
			cache = &backend.NodeCache{Claim: loc.Claim, MountPath: s.CacheMountPath, ScannedAt: snap.ScannedAt, Shared: loc.Shared, Models: countModels(snap.Entries), Inventory: b.opts.InventoryMode}
			for _, e := range snap.Entries {
				cache.BytesUsed += e.Bytes
			}
			if snap.Err != nil {
				cache.Error = snap.Err.Error()
			}
		}
		out = append(out, nodeView(n, reserved[n.Name], cache))
	}
	return out, nil
}

func (b *Backend) labels(extra map[string]string) map[string]string {
	out := map[string]string{ManagedByLabel: ManagedByValue, BackendLabel: "kserve"}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (b *Backend) rememberServed(list []served) {
	b.mu.Lock()
	b.servedCache = list
	b.mu.Unlock()
}

// hubToken reads the configured hub token Secret (cached briefly).
func (b *Backend) hubToken(ctx context.Context) string {
	if b.opts.HFTokenSecret == "" {
		return ""
	}
	b.mu.Lock()
	if time.Since(b.tokenAt) < time.Minute {
		tok := b.token
		b.mu.Unlock()
		return tok
	}
	b.mu.Unlock()
	s := b.cfg.settings(ctx)
	sec, err := b.k8s(ctx).CoreV1().Secrets(s.Namespace).Get(ctx, b.opts.HFTokenSecret, metav1.GetOptions{})
	tok := ""
	if err != nil {
		b.log.Warn("reading the hub token Secret failed", "secret", s.Namespace+"/"+b.opts.HFTokenSecret, "error", err)
	} else {
		tok = strings.TrimSpace(string(sec.Data[b.opts.HFTokenSecretKey]))
	}
	b.mu.Lock()
	b.token, b.tokenAt = tok, time.Now()
	b.mu.Unlock()
	return tok
}

func (b *Backend) tokenConfigured(ctx context.Context) bool {
	return b.hubToken(ctx) != ""
}

// Target is the cluster the backend acts on (backend.Targeter); nil for the
// local cluster.
func (b *Backend) Target() *backend.Target { return b.opts.Target.Identity() }
