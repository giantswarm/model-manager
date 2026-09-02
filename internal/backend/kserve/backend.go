// Package kserve implements the serving backend over KServe: the inventory is
// the per-node Hugging Face cache (a PersistentVolumeClaim scanned by a
// short-lived pod) plus the InferenceServices of the serving namespace; pull
// is a pre-warm download Job into that cache; load composes an
// InferenceService from a curated serving preset (the modelServing contract of
// agent-platform-standalone); unload deletes it. Sizes come from the Hugging
// Face Hub and are fit-checked against node memory budgets before any download
// or start. Agents reach a served model through kagent's OpenAI provider with
// a placeholder API key.
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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/model-manager/internal/backend"
)

// logReader reads a pod's log (replaced in tests: the fake clientset returns
// a fixed body).
type logReader func(ctx context.Context, namespace, name string, tail int64) (string, error)

// Backend is the kserve driver.
type Backend struct {
	opts backend.KServeOptions
	cfg  *config
	cs   kubernetes.Interface
	dyn  dynamic.Interface
	hub  *hubClient
	inv  *inventory
	log  *slog.Logger

	// scan and logs are the node-touching primitives; tests replace them.
	scan scanner
	logs logReader
	// agentHTTP talks to the cache-agent pods (daemonset inventory mode).
	agentHTTP *http.Client

	mu          sync.Mutex
	servedCache []served
	presetCache []*servingPreset
	token       string
	tokenAt     time.Time
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
	case InventoryModePod, InventoryModeDaemonSet:
	default:
		return nil, fmt.Errorf("kserve inventory mode %q: want %s or %s", opts.InventoryMode, InventoryModePod, InventoryModeDaemonSet)
	}
	b := &Backend{
		opts: opts,
		cfg:  newConfig(opts),
		cs:   opts.Clientset,
		dyn:  opts.Dynamic,
		inv:  newInventory(),
		log:  slog.Default().With("backend", "kserve"),
	}
	b.hub = newHubClient(opts.HFEndpoint, &http.Client{Timeout: 30 * time.Second}, b.hubToken)
	b.agentHTTP = &http.Client{Timeout: opts.InventoryTimeout}
	b.scan = b.scanNode
	if opts.InventoryMode == InventoryModeDaemonSet {
		b.scan = b.scanAgent
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

// Info implements backend.Backend: healthy when the InferenceService API
// answers in the serving namespace.
func (b *Backend) Info(ctx context.Context) backend.Info {
	s := b.cfg.settings(ctx)
	info := backend.Info{
		Backend:  backend.NameKServe,
		Version:  isvcGVR.Group + "/" + isvcGVR.Version,
		Endpoint: fmt.Sprintf("%s.%s/%s", isvcGVR.Resource, isvcGVR.Group, s.Namespace),
	}
	if _, err := b.dyn.Resource(isvcGVR).Namespace(s.Namespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		info.Message = fmt.Sprintf("InferenceService API not available in %s: %v", s.Namespace, err)
		return info
	}
	info.Healthy = true
	switch {
	case s.DiscoveryError != "":
		info.Message = "discovery: " + s.DiscoveryError
	case !s.DiscoveryFound:
		info.Message = fmt.Sprintf("no discovery ConfigMap %s/%s; using flags and defaults", b.opts.DiscoveryNamespace, b.opts.DiscoveryConfigMap)
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

	out := make([]backend.Model, 0, len(entries)+len(servedList))
	seen := map[string]bool{} // node/dir
	for _, e := range entries {
		m := b.modelFromEntry(e, idx, servedByName)
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

// modelFromEntry names a cache directory: the marker a pre-warm download
// wrote, the preset of the same name, the InferenceService of the same name,
// else the directory itself.
func (b *Backend) modelFromEntry(e cacheEntry, idx presetIndex, servedByName map[string]served) backend.Model {
	m := backend.Model{
		Node:       e.Node,
		Path:       e.Dir,
		SizeBytes:  e.Bytes,
		ModifiedAt: e.MTime,
		Downloaded: ptr.To(e.Files > 0),
	}
	switch {
	case e.Marker != nil && e.Marker.Model != "":
		m.Name = e.Marker.Model
	case idx.byName[e.Dir] != nil:
		m.Name = idx.byName[e.Dir].Spec.Model.ID
	case servedByName[e.Dir].Model != "":
		m.Name = servedByName[e.Dir].Model
	default:
		m.Name = e.Dir
	}
	if p, ok := idx.byName[e.Dir]; ok {
		m.Preset = p.name()
	} else if matches := idx.forModel(m.Name); len(matches) == 1 {
		m.Preset = matches[0].name()
	}
	if p, ok := idx.byName[m.Preset]; ok {
		m.Format = p.Spec.Model.Format
		m.ContextLength = p.Spec.Model.ContextLength
		m.Capabilities = p.Spec.Model.Capabilities
	}
	if e.Marker != nil && e.Marker.Model != "" {
		m.Digest = e.Marker.Revision
	}
	return m
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
		// Shared storage, or a claim that binds on first use: one scan,
		// wherever the scheduler puts it.
		nodes = []string{""}
	}
	var out []cacheEntry
	for _, node := range nodes {
		snap := b.inv.snapshot(ctx, node, b.opts.InventoryTTL, false, b.scan)
		if snap.Err != nil {
			b.log.Warn("cache scan failed", "node", nodeOrAny(node), "error", snap.Err)
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
// downloaded=false so it can be loaded (the InferenceService downloads).
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

// ListLoaded implements backend.Backend: the InferenceServices.
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
			Status:    sv.Status,
			Message:   sv.Message,
			Resource:  sv.Name,
			Preset:    sv.Preset,
			GPUs:      sv.GPUs,
			ManagedBy: sv.ManagedBy,
		}
		if sv.Deleting {
			lm.Status = "Terminating"
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
// cache directory the model's InferenceService will mount.
func (b *Backend) Pull(ctx context.Context, req backend.PullRequest, progress func(backend.Progress)) error {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return fmt.Errorf("%w: empty model reference", backend.ErrInvalid)
	}
	plan, err := b.fitCheck(ctx, backend.FitRequest{Model: ref, Preset: req.Preset, Node: req.Node}, false)
	if err != nil {
		return err
	}
	res := plan.Result
	if !res.Fits {
		return fmt.Errorf("%w: %s", backend.ErrUnfit, res.Reason)
	}
	if res.Gated && !res.TokenConfigured {
		return fmt.Errorf("%w: %s is gated; configure a Hugging Face token (kserve.hf.tokenSecret) to download it", backend.ErrInvalid, plan.Repo)
	}
	if res.Cached {
		if progress != nil {
			progress(backend.Progress{Status: "already cached", BytesCompleted: res.DownloadBytes, BytesTotal: res.DownloadBytes})
		}
		return nil
	}
	dl := downloadPlan{Repo: plan.Repo, Revision: plan.Revision, Dir: plan.Dir, Preset: res.Preset, BytesTotal: res.DownloadBytes}
	if plan.CacheLocal || req.Node != "" {
		dl.Node = res.Node
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
			return fmt.Errorf("%w: %s is served by InferenceService %s; unload it first", backend.ErrConflict, m.Name, sv.Name)
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
	b.inv.invalidate()
	return nil
}

// Load implements backend.Backend: fit-check, then create the InferenceService
// composed from the preset. Loading the same preset again is a no-op.
func (b *Backend) Load(ctx context.Context, req backend.LoadRequest) error {
	name := strings.TrimSpace(req.Name)
	if name == "" && req.Preset == "" {
		return fmt.Errorf("%w: model or preset is required", backend.ErrInvalid)
	}
	plan, err := b.fitCheck(ctx, backend.FitRequest{Model: name, Preset: req.Preset, Node: req.Node}, true)
	if err != nil {
		return err
	}
	if plan.Preset == nil {
		return fmt.Errorf("%w: no serving preset serves %s; presets are curated in the platform chart (components.modelServing.presets)", backend.ErrInvalid, plan.Repo)
	}
	s := b.cfg.settings(ctx)
	existing, err := b.getISVC(ctx, s.Namespace, plan.Preset.name())
	if err != nil {
		return err
	}
	if existing != nil {
		sv := parseServed(existing, indexPresets([]*servingPreset{plan.Preset}), s.GPUResourceName)
		if sv.manageable() && strings.EqualFold(sv.Model, plan.Repo) {
			b.log.Info("InferenceService already exists", "name", sv.Name, "model", sv.Model, "managedBy", sv.ManagedBy)
			return nil
		}
		return fmt.Errorf("%w: InferenceService %s/%s exists (model %s, managed by %q)", backend.ErrConflict, s.Namespace, sv.Name, sv.Model, sv.ManagedBy)
	}
	if !plan.Result.Fits {
		return fmt.Errorf("%w: %s", backend.ErrUnfit, plan.Result.Reason)
	}
	obj := b.compose(plan.Preset, s, req.Node)
	if err := b.createISVC(ctx, obj); err != nil {
		return err
	}
	b.log.Info("InferenceService created", "name", obj.GetName(), "namespace", s.Namespace, "model", plan.Repo, "preset", plan.Preset.name(), "node", nodeOrAny(req.Node))
	b.inv.invalidate()
	return nil
}

// Unload implements backend.Backend: deletes the InferenceServices serving the
// model; the cache stays. model-manager's own InferenceServices and the ones
// the portal created from a preset are deleted; hand-written ones are not.
func (b *Backend) Unload(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	matches, err := b.servedFor(ctx, name)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("%w: no InferenceService serves %s", backend.ErrNotFound, name)
	}
	for _, sv := range matches {
		if !sv.manageable() {
			return fmt.Errorf("%w: InferenceService %s/%s was not created from a serving preset (managed by %q); delete it where it was created", backend.ErrConflict, sv.Namespace, sv.Name, sv.ManagedBy)
		}
	}
	for _, sv := range matches {
		if err := b.deleteISVC(ctx, sv.Namespace, sv.Name); err != nil {
			return err
		}
		b.log.Info("InferenceService deleted", "name", sv.Name, "namespace", sv.Namespace, "model", sv.Model)
	}
	return nil
}

// servedFor finds the InferenceServices serving a model (by repository id,
// InferenceService name or preset name).
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
		if err != nil {
			b.log.Warn("readiness poll failed", "model", model, "error", err)
		} else if len(matches) == 0 {
			return fmt.Errorf("%w: InferenceService for %s is gone", backend.ErrNotFound, model)
		} else {
			for _, sv := range matches {
				if sv.Ready {
					return nil
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
// the predictor's OpenAI-compatible API. vLLM serves the model under the
// InferenceService name (--served-model-name {{.Name}} in the platform
// runtime) and checks no API key, so the placeholder secret is required. The
// ModelConfig is named after the InferenceService too — the rule the portal's
// serve flow applies, so both wire a served model to the same object.
func (b *Backend) AgentEndpoint(model string) backend.AgentEndpoint {
	repo, _ := splitRevision(model)
	b.mu.Lock()
	servedList := b.servedCache
	presets := b.presetCache
	b.mu.Unlock()
	for _, sv := range servedList {
		if strings.EqualFold(sv.Model, repo) || sv.Name == model {
			return backend.AgentEndpoint{Provider: "OpenAI", BaseURL: sv.URL + "/v1", Model: sv.Name, PlaceholderAPIKey: true, Name: sv.Name}
		}
	}
	name := dnsLabel(repo)
	idx := indexPresets(presets)
	if p, err := idx.resolve(repo, ""); err == nil && p != nil {
		name = p.name()
	}
	ns := b.cfg.last().Namespace
	return backend.AgentEndpoint{Provider: "OpenAI", BaseURL: predictorURL(name, ns) + "/v1", Model: name, PlaceholderAPIKey: true, Name: name}
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
	hits, err := b.hub.Search(ctx, query, limit)
	if err != nil {
		return nil, err
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
	plan, err := b.fitCheck(ctx, req, false)
	if err != nil {
		return nil, err
	}
	res := plan.Result
	return &res, nil
}

// ListNodes implements backend.NodeLister.
func (b *Backend) ListNodes(ctx context.Context) ([]backend.NodeInfo, error) {
	nodes, err := b.nodes(ctx)
	if err != nil {
		return nil, err
	}
	s := b.cfg.settings(ctx)
	presets, _, err := b.presets(ctx)
	if err != nil {
		return nil, err
	}
	reserved := b.reservedByNode(ctx, indexPresets(presets), nil)
	loc, err := b.cacheNodes(ctx)
	if err != nil {
		return nil, err
	}
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
			cache = &backend.NodeCache{Claim: loc.Claim, MountPath: s.CacheMountPath, ScannedAt: snap.ScannedAt, Shared: loc.Shared, Models: len(snap.Entries), Inventory: b.opts.InventoryMode}
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
	sec, err := b.cs.CoreV1().Secrets(s.Namespace).Get(ctx, b.opts.HFTokenSecret, metav1.GetOptions{})
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
